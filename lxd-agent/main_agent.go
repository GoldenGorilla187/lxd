package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/vsock"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
)

type cmdAgent struct {
	global *cmdGlobal
}

func (c *cmdAgent) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "lxd-agent [--debug]"
	cmd.Short = "LXD virtual machine agent"
	cmd.Long = `Description:
  LXD virtual machine agent

  This daemon is to be run inside virtual machines managed by LXD.
  It will normally be started through init scripts present or injected
  into the virtual machine.
`
	cmd.RunE = c.Run

	return cmd
}

func (c *cmdAgent) Run(cmd *cobra.Command, args []string) error {
	// Setup logger.
	err := logger.InitLogger("", "", c.global.flagLogVerbose, c.global.flagLogDebug, nil)
	if err != nil {
		os.Exit(1)
	}

	logger.Info("Starting")
	defer logger.Info("Stopped")

	// Apply the templated files.
	files, err := templatesApply("files/")
	if err != nil {
		return err
	}

	// Sync the hostname.
	if shared.PathExists("/proc/sys/kernel/hostname") && shared.StringInSlice("/etc/hostname", files) {
		// Open the two files.
		src, err := os.Open("/etc/hostname")
		if err != nil {
			return err
		}

		dst, err := os.Create("/proc/sys/kernel/hostname")
		if err != nil {
			return err
		}

		// Copy the data.
		_, err = io.Copy(dst, src)
		if err != nil {
			return err
		}

		// Close the files.
		_ = src.Close()
		err = dst.Close()
		if err != nil {
			return err
		}
	}

	// Run cloud-init.
	if shared.PathExists("/etc/cloud") && shared.StringInSlice("/var/lib/cloud/seed/nocloud-net/meta-data", files) {
		logger.Info("Seeding cloud-init")

		cloudInitPath := "/run/cloud-init"
		if shared.PathExists(cloudInitPath) {
			logger.Info(fmt.Sprintf("Removing %q", cloudInitPath))
			err = os.RemoveAll(cloudInitPath)
			if err != nil {
				return err
			}
		}

		logger.Info("Rebooting")
		_, _ = shared.RunCommand("reboot")

		// Wait up to 5min for the reboot to actually happen, if it doesn't, then move on to allowing connections.
		time.Sleep(300 * time.Second)
	}

	reconfigureNetworkInterfaces()

	// Load the kernel driver.
	logger.Info("Loading vsock module")
	err = util.LoadModule("vsock")
	if err != nil {
		return fmt.Errorf("Unable to load the vsock kernel module: %w", err)
	}

	// Wait for vsock device to appear.
	for i := 0; i < 5; i++ {
		if !shared.PathExists("/dev/vsock") {
			time.Sleep(1 * time.Second)
		}
	}

	// Setup the listener.
	l, err := vsock.Listen(shared.HTTPSDefaultPort)
	if err != nil {
		return fmt.Errorf("Failed to listen on vsock: %w", err)
	}

	logger.Info("Started vsock listener")

	// Mount shares from host.
	c.mountHostShares()

	// Done with early setup, tell systemd to continue boot.
	// Allows a service that needs a file that's generated by the agent to be able to declare After=lxd-agent
	// and know the file will have been created by the time the service is started.
	if os.Getenv("NOTIFY_SOCKET") != "" {
		_, err := shared.RunCommand("systemd-notify", "READY=1")
		if err != nil {
			return fmt.Errorf("Failed to notify systemd of readiness: %w", err)
		}
	}

	// Load the expected server certificate.
	cert, err := shared.ReadCert("server.crt")
	if err != nil {
		return fmt.Errorf("Failed to read client certificate: %w", err)
	}

	tlsConfig, err := serverTLSConfig()
	if err != nil {
		return fmt.Errorf("Failed to get TLS config: %w", err)
	}

	d := newDaemon(c.global.flagLogDebug, c.global.flagLogVerbose)

	servers := make(map[string]*http.Server, 2)

	// Prepare the HTTP server.
	servers["http"] = restServer(tlsConfig, cert, c.global.flagLogDebug, d)

	// Prepare the devlxd server.
	devlxdListener, err := createDevLxdlListener("/dev")
	if err != nil {
		return err
	}

	servers["devlxd"] = devLxdServer(d)

	// Create a cancellation context.
	ctx, cancelFunc := context.WithCancel(context.Background())

	// Start status notifier in background.
	cancelStatusNotifier := c.startStatusNotifier(ctx)

	errChan := make(chan error, 1)

	// Start the server.
	go func() {
		err := servers["http"].Serve(networkTLSListener(l, tlsConfig))
		if err != nil {
			errChan <- err
		}
	}()

	// Only start the devlxd listener if instance-data is present.
	if shared.PathExists("instance-data") {
		go func() {
			err := servers["devlxd"].Serve(devlxdListener)
			if err != nil {
				errChan <- err
			}
		}()
	}

	// Cancel context when SIGTEM is received.
	chSignal := make(chan os.Signal, 1)
	signal.Notify(chSignal, unix.SIGTERM)

	exitStatus := 0

	select {
	case <-chSignal:
	case err := <-errChan:
		fmt.Fprintln(os.Stderr, err)
		exitStatus = 1
	}

	cancelStatusNotifier() // Ensure STOPPED status is written to QEMU status ringbuffer.
	cancelFunc()

	os.Exit(exitStatus)

	return nil
}

// startStatusNotifier sends status of agent to vserial ring buffer every 10s or when context is done.
// Returns a function that can be used to update the running status to STOPPED in the ring buffer.
func (c *cmdAgent) startStatusNotifier(ctx context.Context) context.CancelFunc {
	// Write initial started status.
	_ = c.writeStatus("STARTED")

	wg := sync.WaitGroup{}
	exitCtx, exit := context.WithCancel(ctx) // Allows manual synchronous cancellation via cancel function.
	cancel := func() {
		exit()    // Signal for the go routine to end.
		wg.Wait() // Wait for the go routine to actually finish.
	}

	wg.Add(1)
	go func() {
		defer wg.Done() // Signal to cancel function that we are done.

		ticker := time.NewTicker(time.Duration(time.Second) * 5)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = c.writeStatus("STARTED") // Re-populate status periodically in case LXD restarts.
			case <-exitCtx.Done():
				_ = c.writeStatus("STOPPED") // Indicate we are stopping to LXD and exit go routine.
				return
			}
		}
	}()

	return cancel
}

// writeStatus writes a status code to the vserial ring buffer used to detect agent status on host.
func (c *cmdAgent) writeStatus(status string) error {
	if shared.PathExists("/dev/virtio-ports/org.linuxcontainers.lxd") {
		vSerial, err := os.OpenFile("/dev/virtio-ports/org.linuxcontainers.lxd", os.O_RDWR, 0600)
		if err != nil {
			return err
		}

		defer vSerial.Close()

		_, err = vSerial.Write([]byte(fmt.Sprintf("%s\n", status)))
		if err != nil {
			return err
		}
	}

	return nil
}

// mountHostShares reads the agent-mounts.json file from config share and mounts the shares requested.
func (c *cmdAgent) mountHostShares() {
	agentMountsFile := "./agent-mounts.json"
	if !shared.PathExists(agentMountsFile) {
		return
	}

	b, err := ioutil.ReadFile(agentMountsFile)
	if err != nil {
		logger.Errorf("Failed to load agent mounts file %q: %v", agentMountsFile, err)
	}

	var agentMounts []instancetype.VMAgentMount
	err = json.Unmarshal(b, &agentMounts)
	if err != nil {
		logger.Errorf("Failed to parse agent mounts file %q: %v", agentMountsFile, err)
		return
	}

	for _, mount := range agentMounts {
		// Convert relative mounts to absolute from / otherwise dir creation fails or mount fails.
		if !strings.HasPrefix(mount.Target, "/") {
			mount.Target = fmt.Sprintf("/%s", mount.Target)
		}

		if !shared.PathExists(mount.Target) {
			err := os.MkdirAll(mount.Target, 0755)
			if err != nil {
				logger.Errorf("Failed to create mount target %q", mount.Target)
				continue // Don't try to mount if mount point can't be created.
			}
		}

		if mount.FSType == "9p" {
			// Before mounting with 9p, try virtio-fs and use 9p as the fallback.
			args := []string{"-t", "virtiofs", mount.Source, mount.Target}

			for _, opt := range mount.Options {
				// Ignore the transport and msize mount option as they are specific to 9p.
				if strings.HasPrefix(opt, "trans=") || strings.HasPrefix(opt, "msize=") {
					continue
				}

				args = append(args, "-o", opt)
			}

			_, err = shared.RunCommand("mount", args...)
			if err == nil {
				logger.Infof("Mounted %q (Type: %q, Options: %v) to %q", mount.Source, "virtiofs", mount.Options, mount.Target)
				continue
			}
		}

		args := []string{"-t", mount.FSType, mount.Source, mount.Target}

		for _, opt := range mount.Options {
			args = append(args, "-o", opt)
		}

		_, err = shared.RunCommand("mount", args...)
		if err != nil {
			logger.Errorf("Failed mount %q (Type: %q, Options: %v) to %q: %v", mount.Source, mount.FSType, mount.Options, mount.Target, err)
			continue
		}

		logger.Infof("Mounted %q (Type: %q, Options: %v) to %q", mount.Source, mount.FSType, mount.Options, mount.Target)
	}
}
