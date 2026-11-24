// Package rabbitmqtest runs integration tests against a real RabbitMQ in a
// throwaway container, loaded with the topology, users and permissions that
// Docker Compose loads, each user with a random password. Import it only
// from tests.
package rabbitmqtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/thiagogenez/labflow/libs/go/dockertest"
	"github.com/thiagogenez/labflow/libs/go/rabbitmq"
)

// image matches the RabbitMQ image in Docker Compose.
const image = "rabbitmq:4.4.1-management@sha256:61f90c401a5b5ed2486771973ea92ad66046471eacab9c9149f1a6e091c91677"

// Vhost is the application virtual host in the definitions.
const Vhost = "labflow"

var (
	ctr       *testcontainers.DockerContainer
	address   string
	passwords = map[string]string{}
)

// Main starts one broker, calls run and removes the broker. Use it as
// os.Exit(rabbitmqtest.Main(m.Run)), or wrap pgtest.Main to get both.
func Main(run func() int) int {
	dockertest.DisableReaper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	var err error
	ctr, err = start(ctx)
	// Run may return a container together with an error; remove it either way.
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintln(os.Stderr, "rabbitmqtest: remove rabbitmq container:", err)
		}
	}()
	if err == nil {
		address, err = ctr.PortEndpoint(ctx, "5672/tcp", "")
	}
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rabbitmqtest: start rabbitmq container:", err)
		return 1
	}
	return run()
}

func start(ctx context.Context) (*testcontainers.DockerContainer, error) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "compose", "rabbitmq")
	definitions, err := render(filepath.Join(dir, "definitions.template.json"))
	if err != nil {
		return nil, err
	}
	return testcontainers.Run(ctx, image,
		testcontainers.WithExposedPorts("5672/tcp"),
		dockertest.Isolate("5672/tcp"),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: filepath.Join(dir, "labflow.conf"), ContainerFilePath: "/etc/rabbitmq/conf.d/90-labflow.conf", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(definitions), ContainerFilePath: "/run/secrets/rabbitmq-definitions.json", FileMode: 0o644},
		),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("Server startup complete"),
			wait.ForListeningPort("5672/tcp"),
		).WithDeadline(2*time.Minute)),
	)
}

// URL returns the AMQP URL, with its password, for a user from the
// definitions.
func URL(t *testing.T, user string) string {
	t.Helper()
	if address == "" {
		t.Fatal("rabbitmqtest: call rabbitmqtest.Main from TestMain")
	}
	password, ok := passwords[user]
	if !ok {
		t.Fatalf("rabbitmqtest: no user %q in the definitions", user)
	}
	return fmt.Sprintf("amqp://%s:%s@%s/%s", user, password, address, Vhost)
}

// Admin returns a channel with every permission on the vhost, for setting up
// and inspecting a test.
func Admin(t *testing.T) *amqp.Channel {
	t.Helper()
	conn, err := amqp.Dial(URL(t, "admin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// Purge empties the queues, so each test starts from a known state.
func Purge(t *testing.T, queues ...string) {
	t.Helper()
	ch := Admin(t)
	for _, q := range queues {
		if _, err := ch.QueuePurge(q, false); err != nil {
			t.Fatal(err)
		}
	}
}

// Get takes the next message from a queue, failing the test when there is
// none within a few seconds.
func Get(t *testing.T, queue string) amqp.Delivery {
	t.Helper()
	ch := Admin(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, ok, err := ch.Get(queue, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message on %s", queue)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exec runs a command in the broker container, such as rabbitmqctl, and
// returns its output, failing the test when it does not exit 0. It works in
// t.Cleanup, after the test's context has ended.
func Exec(t *testing.T, cmd ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code, out, err := ctr.Exec(ctx, cmd, tcexec.Multiplexed())
	var output []byte
	if out != nil {
		output, _ = io.ReadAll(out)
	}
	if err != nil || code != 0 {
		t.Fatalf("%v: exit %d, %v: %s", cmd, code, err, output)
	}
	return string(output)
}

// Pause freezes the broker until the test ends, as with a broker that hangs.
func Pause(t *testing.T) {
	t.Helper()
	dockertest.Pause(t, ctr.GetContainerID())
}

// render gives every user in the definitions template a random password,
// with the renderer the local setup uses.
func render(path string) ([]byte, error) {
	template, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	users, err := rabbitmq.DefinitionUsers(template)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		passwords[user] = rand.Text() + rand.Text()
	}
	return rabbitmq.RenderDefinitions(template, passwords)
}
