// Package dockertest holds helpers for tests that run containers. Import it
// only from tests.
package dockertest

import (
	"context"
	"testing"

	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

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
