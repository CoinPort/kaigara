package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// runAndWait runs a shell snippet to completion and returns the error that
// exec.Cmd.Wait reported, so exitCode can be exercised against real statuses
// rather than hand-built error values.
func runAndWait(t *testing.T, script string) error {
	t.Helper()

	c := exec.Command("sh", "-c", script)
	assert.NoError(t, c.Start())

	return c.Wait()
}

func TestExitCodeSuccess(t *testing.T) {
	assert.Equal(t, 0, exitCode(nil))
	assert.Equal(t, 0, exitCode(runAndWait(t, "exit 0")))
}

func TestExitCodePropagatesChildStatus(t *testing.T) {
	// The whole point of the change: a wrapped daemon exiting 7 must surface
	// as 7, not as the blanket 1 that log.Fatal used to produce.
	for _, want := range []int{1, 7, 42} {
		assert.Equal(t, want, exitCode(runAndWait(t, "exit "+itoa(want))))
	}
}

func TestExitCodeSignalledChild(t *testing.T) {
	// Shell convention: 128 + signal number.
	assert.Equal(t, 128+int(syscall.SIGKILL), exitCode(runAndWait(t, "kill -KILL $$")))
	assert.Equal(t, 128+int(syscall.SIGTERM), exitCode(runAndWait(t, "kill -TERM $$")))
}

func TestExitCodeNonExitError(t *testing.T) {
	assert.Equal(t, 1, exitCode(os.ErrNotExist))
}

// superviseFixture starts a child and wires up the channels the supervisor
// expects, mirroring how kaigaraRun drives it.
func superviseFixture(t *testing.T, script string) (*exec.Cmd, chan error, chan os.Signal, chan string) {
	t.Helper()

	c := exec.Command("sh", "-c", script)
	assert.NoError(t, c.Start())

	waitDone := make(chan error, 1)
	go func() { waitDone <- c.Wait() }()

	return c, waitDone, make(chan os.Signal, 1), make(chan string, 1)
}

func TestSuperviseChildReturnsWhenChildExits(t *testing.T) {
	c, waitDone, sigCh, restart := superviseFixture(t, "exit 3")

	done := make(chan error, 1)
	go func() { done <- superviseChild(c, waitDone, sigCh, restart) }()

	select {
	case err := <-done:
		assert.Equal(t, 3, exitCode(err))
	case <-time.After(5 * time.Second):
		t.Fatal("superviseChild did not return after the child exited")
	}
}

func TestSuperviseChildForwardsSignal(t *testing.T) {
	// The child traps SIGTERM and exits 17, which only happens if the signal
	// was actually delivered to it rather than consumed by Kaigara.
	c, waitDone, sigCh, restart := superviseFixture(t,
		"trap 'exit 17' TERM; while true; do sleep 0.1; done")

	done := make(chan error, 1)
	go func() { done <- superviseChild(c, waitDone, sigCh, restart) }()

	time.Sleep(300 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case err := <-done:
		assert.Equal(t, 17, exitCode(err), "child should have handled the forwarded SIGTERM")
	case <-time.After(5 * time.Second):
		t.Fatal("superviseChild did not return after forwarding SIGTERM")
	}
}

func TestSuperviseChildRestartRequestIsGraceful(t *testing.T) {
	// A secret change must give the daemon a chance to shut down cleanly,
	// which the old c.Process.Kill() never did.
	c, waitDone, sigCh, restart := superviseFixture(t,
		"trap 'exit 23' TERM; while true; do sleep 0.1; done")

	done := make(chan error, 1)
	go func() { done <- superviseChild(c, waitDone, sigCh, restart) }()

	time.Sleep(300 * time.Millisecond)
	restart <- "Secrets updated on peatio.private: v1 -> v2"

	select {
	case err := <-done:
		assert.Equal(t, 23, exitCode(err), "restart should SIGTERM, not SIGKILL")
	case <-time.After(5 * time.Second):
		t.Fatal("superviseChild did not return after a restart request")
	}
}

func TestPollIntervalStaysInWindow(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := pollInterval()
		assert.GreaterOrEqual(t, d, pollBase)
		assert.Less(t, d, pollBase+pollJitter)
		seen[d] = true
	}

	// If the jitter were dropped, every service would poll on the same tick
	// and restart together.
	assert.Greater(t, len(seen), 1, "poll interval should vary")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}

	return string(b)
}
