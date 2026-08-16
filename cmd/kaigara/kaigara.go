package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/CoinPort/kaigara/pkg/config"
	"github.com/CoinPort/kaigara/pkg/ika"
	"github.com/CoinPort/kaigara/pkg/logstream"
	"github.com/CoinPort/kaigara/types"
)

const (
	// shutdownGrace is how long the child gets to exit after SIGTERM before
	// Kaigara escalates to SIGKILL. Kept under Docker's default 10s stop
	// timeout so the daemon, not the container runtime, decides how it stops.
	shutdownGrace = 8 * time.Second

	// pollBase and pollJitter set the window for the secret version check.
	// The jitter keeps every wrapped service from restarting on the same tick
	// after a single kaisave run.
	pollBase   = 20 * time.Second
	pollJitter = 10 * time.Second
)

var cnf = &config.KaigaraConfig{}

func parseScopes() []string {
	return strings.Split(cnf.Scopes, ",")
}

func parseAppNames() []string {
	return strings.Split(cnf.AppNames, ",")
}

func appNamesToLoggingName() string {
	return strings.Join(parseAppNames(), "&")
}

// kaigaraRun starts the wrapped command and returns the exit status it should
// be reported as.
func kaigaraRun(ls logstream.LogStream, store types.Storage, cmd string, cmdArgs []string) int {
	log.Printf("Starting command: %s %v", cmd, cmdArgs)
	scopes := parseScopes()
	c := exec.Command(cmd, cmdArgs...)
	env := config.BuildCmdEnv(parseAppNames(), store, os.Environ(), scopes)

	c.Env = env.Vars

	for _, file := range env.Files {
		err := os.MkdirAll(path.Dir(file.Path), 0750)
		if err != nil {
			panic(fmt.Sprintf("Failed to make dir %s: %s", file.Path, err.Error()))
		}

		err = ioutil.WriteFile(file.Path, []byte(file.Content), 0640)
		if err != nil {
			panic(fmt.Sprintf("Failed to write file %s: %s", file.Path, err.Error()))
		}
	}

	stdin, err := c.StdinPipe()
	if err != nil {
		panic(err)
	}

	// Read STDIN and write it to the command
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			line, isPrefix, err := r.ReadLine()
			if err == io.EOF {
				log.Printf("Reached EOF on STDIN")
				stdin.Close()
				break
			} else if err != nil {
				panic(err)
			}
			_, err = stdin.Write(line)
			if err != nil {
				panic(err)
			}

			if !isPrefix {
				_, err = stdin.Write([]byte("\n"))
				if err != nil {
					panic(err)
				}
			}
		}
	}()

	stdout, err := c.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	stderr, err := c.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}

	channelOut := fmt.Sprintf("log.%s.%s", appNamesToLoggingName(), "stdout")
	channelErr := fmt.Sprintf("log.%s.%s", appNamesToLoggingName(), "stderr")
	log.Printf("Publishing on %s and %s\n", channelOut, channelErr)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		ls.Publish(channelOut, stdout)
		wg.Done()
	}()

	go func() {
		ls.Publish(channelErr, stderr)
		wg.Done()
	}()

	if err := c.Start(); err != nil {
		log.Fatal(err)
	}

	// Only one goroutine may call Wait, so the result is funnelled through a
	// channel and the select below is the single place that learns the child
	// has exited.
	waitDone := make(chan error, 1)
	go func() { waitDone <- c.Wait() }()

	// Forward termination signals to the child. Without this, stopping the
	// container stops Kaigara and orphans the daemon, which then gets killed
	// by the runtime after the stop timeout with no chance to drain.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// watchSecrets polls on a ticker until it is told to stop. Without this
	// signal the goroutine outlives the call: kaigaraRun returns as soon as
	// the child exits, but the ticker keeps firing and the poller keeps
	// reading the package-level cnf that the next call may already be
	// writing. One leaked poller per restart, and a data race with it.
	watchDone := make(chan struct{})
	defer close(watchDone)

	restart := make(chan string, 1)
	// parseAppNames is called here rather than inside watchSecrets so the
	// package-level cnf is read on this goroutine, before the poller starts.
	// Closing watchDone asks the poller to stop but does not wait for it, so
	// a read left inside it would still be live after kaigaraRun returns.
	go watchSecrets(store, parseAppNames(), scopes, restart, watchDone)

	quit := make(chan int)
	go func() {
		ls.HeartBeat(appNamesToLoggingName(), quit)
		wg.Done()
	}()

	waitErr := superviseChild(c, waitDone, sigCh, restart)

	quit <- 0
	wg.Wait()

	code := exitCode(waitErr)
	log.Printf("exit status %d\n", code)

	return code
}

// superviseChild blocks until the child exits, relaying signals and
// stop requests to it, and returns whatever c.Wait reported.
func superviseChild(c *exec.Cmd, waitDone chan error, sigCh chan os.Signal, restart chan string) error {
	// A nil channel blocks forever, so the kill deadline stays disarmed until
	// something actually asks the child to stop.
	var killDeadline <-chan time.Time

	for {
		select {
		case err := <-waitDone:
			return err

		case sig := <-sigCh:
			log.Printf("Received %s, forwarding to pid %d", sig, c.Process.Pid)
			if err := c.Process.Signal(sig); err != nil {
				log.Printf("ERR: failed to forward %s: %v", sig, err)
			}
			// SIGHUP is a reload hint for many daemons, so it does not start
			// the shutdown clock.
			if sig != syscall.SIGHUP && killDeadline == nil {
				killDeadline = time.After(shutdownGrace)
			}

		case reason := <-restart:
			log.Printf("%s; asking pid %d to stop", reason, c.Process.Pid)
			if err := c.Process.Signal(syscall.SIGTERM); err != nil {
				log.Printf("ERR: failed to signal pid %d: %v", c.Process.Pid, err)
			}
			if killDeadline == nil {
				killDeadline = time.After(shutdownGrace)
			}

		case <-killDeadline:
			log.Printf("Child did not exit within %s, sending SIGKILL", shutdownGrace)
			if err := c.Process.Kill(); err != nil {
				log.Printf("ERR: failed to kill pid %d: %v", c.Process.Pid, err)
			}
			killDeadline = nil
		}
	}
}

// exitCode maps the error from exec.Cmd.Wait onto a process exit status so the
// supervisor can tell a clean exit from a crash. Signalled children follow the
// shell convention of 128+signal.
func exitCode(err error) int {
	if err == nil {
		return 0
	}

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		log.Printf("ERR: %v", err)
		return 1
	}

	if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}

	if code := ee.ExitCode(); code >= 0 {
		return code
	}

	return 1
}

func initLogStream() logstream.LogStream {
	url := os.Getenv("KAIGARA_REDIS_URL")
	return logstream.NewRedisClient(url)
}

// watchSecrets polls the store and reports the first scope whose version has
// moved on. It does not stop the child itself; the supervisor loop owns that,
// so the shutdown is graceful and there is one place that signals the process.
func watchSecrets(store types.Storage, appNames, scopes []string, restart chan<- string, done <-chan struct{}) {
	if ignore, ok := os.LookupEnv("KAIGARA_IGNORE_GLOBAL"); !ok || ignore != "true" {
		// Copy rather than append in place: the caller's slice may share a
		// backing array, and this poller must not write through it.
		watched := make([]string, len(appNames), len(appNames)+1)
		copy(watched, appNames)
		appNames = append(watched, "global")
	}

	ticker := time.NewTicker(pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		for _, appName := range appNames {
			for _, scope := range scopes {
				current, err := store.GetCurrentVersion(appName, scope)
				if err != nil {
					log.Println(err.Error())
					break
				}
				latest, err := store.GetLatestVersion(appName, scope)
				if err != nil {
					log.Println(err.Error())
					break
				}
				if current != latest {
					restart <- fmt.Sprintf(
						"Secrets updated on %s.%s: v%v -> v%v", appName, scope, current, latest)
					return
				}
			}
		}
	}
}

// seedJitter gives each process its own jitter sequence. Before Go 1.20 the
// global source is seeded to a constant, which would hand every wrapped
// service an identical "random" poll interval and defeat the spreading.
func seedJitter() {
	rand.Seed(time.Now().UnixNano() ^ int64(os.Getpid()))
}

// pollInterval spreads the version check over a window so that a single
// kaisave run does not restart every wrapped service on the same tick.
func pollInterval() time.Duration {
	jitter := time.Duration(rand.Int63n(int64(pollJitter)))
	return pollBase + jitter
}

func main() {
	log.SetPrefix("[Kaigara] ")
	seedJitter()
	if len(os.Args) < 2 {
		panic("Usage: kaigara CMD [ARGS...]")
	}
	ls := initLogStream()

	if err := ika.ReadConfig("", cnf); err != nil {
		panic(err)
	}

	store, err := config.GetStorageService(cnf)
	if err != nil {
		panic(err)
	}

	// Propagate the child's exit status. Reporting 0 or 1 regardless leaves
	// supervisors unable to tell a clean shutdown from a crash.
	os.Exit(kaigaraRun(ls, store, os.Args[1], os.Args[2:]))
}
