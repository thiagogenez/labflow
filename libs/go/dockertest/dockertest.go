// Package dockertest holds helpers for tests that run containers. Import it
// only from tests.
package dockertest

import (
	"context"
	"net/netip"
	"os"
	"strconv"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// Isolate publishes the container ports on 127.0.0.1 only, so no other
// machine can reach a test server while it runs, and labels the container
// labflow.test with the test process's ID, so `make test` removes it once
// that process is gone, if a killed run left it behind.
func Isolate(ports ...string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		if err := testcontainers.WithLabels(map[string]string{"labflow.test": strconv.Itoa(os.Getpid())})(req); err != nil {
			return err
		}
		return localhost(ports)(req)
	}
}

func localhost(ports []string) testcontainers.CustomizeRequestOption {
	return testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
		if hc.PortBindings == nil {
			hc.PortBindings = network.PortMap{}
		}
		for _, p := range ports {
			hc.PortBindings[network.MustParsePort(p)] = []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}}
		}
	})
}

// DisableReaper turns off Testcontainers' reaper, which would publish a
// port on every interface while holding the Docker socket; the helpers remove
// their own containers. Call it before the first container starts.
func DisableReaper() { _ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true") }

// Pause freezes a container until the test ends: its connections stay open
// and the kernel still accepts bytes, but nothing answers, as with a server
// that hangs.
func Pause(t *testing.T, id string) {
	t.Helper()
	cli, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ContainerPause(t.Context(), id, client.ContainerPauseOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := cli.ContainerUnpause(context.Background(), id, client.ContainerUnpauseOptions{}); err != nil {
			t.Error(err)
		}
		_ = cli.Close()
	})
}
