package pgtest

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// The test server gets its password as a file, never in the container's
// environment, which container inspection shows.
func TestPasswordStaysOutOfTheEnvironment(t *testing.T) {
	inspect, err := ctr.Inspect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range inspect.Config.Env {
		if strings.HasPrefix(kv, "POSTGRES_PASSWORD=") {
			t.Errorf("container environment holds %s=…", strings.SplitN(kv, "=", 2)[0])
		}
	}
	NewDatabase(t) // the password from the file works
}
