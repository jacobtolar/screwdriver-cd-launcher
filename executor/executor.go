package executor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/screwdriver-cd/launcher/screwdriver"
)

const (
	// ExitLaunch is the exit code when a step fails to launch
	ExitLaunch = 255
	// ExitUnknown is the exit code when a step doesn't return an exit code (for some weird reason)
	ExitUnknown = 254
	// ExitOk is the exit code when a step runs successfully
	ExitOk = 0
	// How long should wait for the env file
	WaitTimeout = 5
)

// ErrStatus is an error that holds an exit status code
type ErrStatus struct {
	Status int
}

func (e ErrStatus) Error() string {
	return fmt.Sprintf("exit %d", e.Status)
}

// Create a sh file
func createShFile(path string, cmd screwdriver.CommandDef, shellBin string) error {
	return ioutil.WriteFile(path, []byte("#!"+shellBin+" -e\n"+cmd.Cmd), 0755)
}

// Returns a single line (without the ending \n) from the input buffered reader
// Pulled from https://stackoverflow.com/a/12206365
func readln(r *bufio.Reader) (string, error) {
	var (
		isPrefix = true
		err      error
		line, ln []byte
	)

	for isPrefix && err == nil {
		line, isPrefix, err = r.ReadLine()
		ln = append(ln, line...)
	}

	return string(ln), err
}

// Copy lines until match string
func copyLinesUntil(r io.Reader, w io.Writer, match string) (int, error) {
	var (
		err    error
		t      string
		reader = bufio.NewReader(r)
		// Match the guid and exitCode
		reExit = regexp.MustCompile(fmt.Sprintf("(%s) ([0-9]+)", match))
		// Match the export SD_STEP_ID command
		reExport = regexp.MustCompile("export SD_STEP_ID=(" + match + ")")
	)
	t, err = readln(reader)
	for err == nil {
		parts := reExit.FindStringSubmatch(t)
		if len(parts) != 0 {
			exitCode, rerr := strconv.Atoi(parts[2])
			if rerr != nil {
				return ExitUnknown, fmt.Errorf("Error converting the exit code to int: %v", rerr)
			}
			if exitCode != 0 {
				return exitCode, fmt.Errorf("Launching command exit with code: %v", exitCode)
			}
			return ExitOk, nil
		}
		// Filter out the export command from the output
		exportCmd := reExport.FindStringSubmatch(t)
		if len(exportCmd) == 0 {
			_, werr := fmt.Fprintln(w, t)
			if werr != nil {
				return ExitUnknown, fmt.Errorf("Error piping logs to emitter: %v", werr)
			}
		}

		t, err = readln(reader)
	}
	if err != nil {
		return ExitUnknown, fmt.Errorf("Error with reader: %v", err)
	}
	return ExitOk, nil
}

func doRunSetupCommand(emitter screwdriver.Emitter, f *os.File, r io.Reader, setupCommands []string) error {
	var (
		t      string
		err    error
		reader = bufio.NewReader(r)
		reEcho = regexp.MustCompile("echo ;")
	)

	shargs := strings.Join(setupCommands, " && ")

	f.Write([]byte(shargs))

	t, err = readln(reader)
	for err == nil {
		echoCmd := reEcho.FindStringSubmatch(t)
		if len(echoCmd) != 0 {
			_, werr := fmt.Fprintln(emitter, t)
			if werr != nil {
				return fmt.Errorf("Error piping logs to emitter: %v", werr)
			}
			return nil
		}
		_, werr := fmt.Fprintln(emitter, t)
		if werr != nil {
			return fmt.Errorf("Error piping logs to emitter: %v", werr)
		}
		t, err = readln(reader)
	}
	if err != nil {
		return fmt.Errorf("Error with reader: %v", err)
	}
	return nil
}

func doRunCommand(guid, path string, emitter screwdriver.Emitter, f *os.File, fReader io.Reader) (int, error) {
	executionCommand := []string{
		"export SD_STEP_ID=" + guid,
		";. " + path,
		";echo",
		";echo " + guid + " $?\n",
	}
	shargs := strings.Join(executionCommand, " ")

	f.Write([]byte(shargs))

	return copyLinesUntil(fReader, emitter, guid)
}

// Executes teardown commands
func doRunTeardownCommand(cmd screwdriver.CommandDef, emitter screwdriver.Emitter, shellBin, exportFile, sourceDir string, stepExitCode int) (int, error) {
	shargs := []string{"-e", "-c"}
	cmdStr := "export PATH=${PATH}:/opt/sd:/usr/sd/bin SD_STEP_EXIT_CODE=" + strconv.Itoa(stepExitCode) + " && " +
		"START=$(date +'%s'); while ! [ -f " + exportFile + " ] && [ $(($(date +'%s')-$START)) -lt " + strconv.Itoa(WaitTimeout) + " ]; do sleep 1; done; " +
		"if [ -f " + exportFile + " ]; then set +e; . " + exportFile + "; set -e; fi; " +
		cmd.Cmd

	shargs = append(shargs, cmdStr)

	c := exec.Command(shellBin, shargs...)
	emitter.StartCmd(cmd)
	fmt.Fprintf(emitter, "$ %s\n", cmd.Cmd)
	c.Stdout = emitter
	c.Stderr = emitter
	c.Dir = sourceDir

	if err := c.Start(); err != nil {
		return ExitLaunch, fmt.Errorf("Launching command %q: %v", cmd.Cmd, err)
	}

	if err := c.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)

			return waitStatus.ExitStatus(), ErrStatus{waitStatus.ExitStatus()}
		}

		return ExitUnknown, fmt.Errorf("Running command %q: %v", cmd.Cmd, err)
	}

	return ExitOk, nil
}

// Initiate the build timeout timer
func initBuildTimeout(timeout time.Duration, ch chan<- error) {
	log.Printf("Starting timer for timeout of %v seconds", timeout)
	time.Sleep(timeout)
	log.Printf("Timeout of %v seconds exceeded. Signal kill-build process", timeout)
	ch <- fmt.Errorf("Timeout of %v seconds exceeded", timeout)
}

// trap sigterm signal and handle it
func notifySignal(sigs chan os.Signal, ch chan<- error) {
	sig := <-sigs
	fmt.Printf("Received %s signal in launcher, processing signal \n", sig)
	ch <- fmt.Errorf("SIGTERM received, step aborted")
}

// print timeout message to build & kill shell
func handleBuildTimeout(f *os.File, timeoutErr error) {
	l := []string{
		"#####################################################################",
		"#####################################################################",
		"#####################################################################",
		" _     _                                      _ ",
		"| |   (_)                                    | |",
		"| |_   _   _ __ ___     ___    ___    _   _  | |_ ",
		"| __| | | | '_ ` _ \\   / _ \\  / _ \\  | | | | | __|",
		"| |_  | | | | | | | | |  __/ | (_) | | |_| | | |_ ",
		" \\__| |_| |_| |_| |_|  \\___|  \\___/   \\__,_|  \\__|",
		"",
		fmt.Sprintf("%v\n", timeoutErr.Error()),
		"",
		"#####################################################################",
		"#####################################################################",
		"#####################################################################",
	}

	for _, msg := range l {
		// print lines
		f.Write([]byte(fmt.Sprintf("%v\n", msg)))
	}

	// kill shell
	f.Write([]byte{4})
}

func filterTeardowns(build screwdriver.Build) ([]screwdriver.CommandDef, []screwdriver.CommandDef, []screwdriver.CommandDef) {
	userCommands := []screwdriver.CommandDef{}
	sdTeardownCommands := []screwdriver.CommandDef{}
	userTeardownCommands := []screwdriver.CommandDef{}

	for _, cmd := range build.Commands {
		isSdTeardown, _ := regexp.MatchString("^sd-teardown-.+", cmd.Name)
		isUserTeardown, _ := regexp.MatchString("^(pre|post)?teardown-.+", cmd.Name)

		if isSdTeardown {
			sdTeardownCommands = append(sdTeardownCommands, cmd)
		} else if isUserTeardown {
			userTeardownCommands = append(userTeardownCommands, cmd)
		} else {
			userCommands = append(userCommands, cmd)
		}
	}

	return userCommands, sdTeardownCommands, userTeardownCommands
}

// Run executes a slice of CommandDefs
func Run(path string, env []string, emitter screwdriver.Emitter, build screwdriver.Build, api screwdriver.API, buildID int, shellBin string, timeoutSec int, envFilepath, sourceDir string) error {
	tmpFile := envFilepath + "_tmp"
	exportFile := envFilepath + "_export"

	// Set up a single pseudo-terminal
	c := exec.Command(shellBin)
	c.Dir = path
	c.Env = append(env, c.Env...)

	f, err := pty.Start(c)
	if err != nil {
		return fmt.Errorf("Cannot start shell: %v", err)
	}

	// Command to Export Env. Use tmpfile just in case export -p takes some time
	exportEnvCmd :=
		"tmpfile=" + tmpFile + "; exportfile=" + exportFile + "; " +
			"export -p | grep -vi \"PS1=\" > $tmpfile && mv -f $tmpfile $exportfile; "

	// Run setup commands
	setupCommands := []string{
		"set -e",
		"export PATH=${PATH}:/opt/sd:/usr/sd/bin",
		// trap ABRT(6) and EXIT, echo the last step ID and write ENV to /tmp/buildEnv
		"finish() { " +
			"EXITCODE=$?; " +
			exportEnvCmd +
			"echo $SD_STEP_ID $EXITCODE; }", //mv newfile to file
		"trap finish ABRT EXIT;\necho ;\n",
	}

	setupReader := bufio.NewReader(f)
	if err := doRunSetupCommand(emitter, f, setupReader, setupCommands); err != nil {
		return err
	}

	var firstError error
	var code int
	var stepExitCode int
	var cmdErr error

	timeout := time.Duration(timeoutSec) * time.Second
	invokeTimeout := make(chan error, 1)
	sig := make(chan error, 1)

	// add a SIGTERM signal handler
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// start build timeout timer
	go initBuildTimeout(timeout, invokeTimeout)
	go notifySignal(sigs, sig)

	userCommands, sdTeardownCommands, userTeardownCommands := filterTeardowns(build)

	for _, cmd := range userCommands {
		// Start set up & user steps if previous steps succeed
		if firstError != nil {
			break
		}

		if err := api.UpdateStepStart(buildID, cmd.Name); err != nil {
			return fmt.Errorf("Updating step start %q: %v", cmd.Name, err)
		}

		// Create step script file
		stepFilePath := "/tmp/step.sh"
		if err := createShFile(stepFilePath, cmd, shellBin); err != nil {
			return fmt.Errorf("Writing to step script file: %v", err)
		}

		// Generate guid v4 for the step
		guid := uuid.Must(uuid.NewRandom()).String()

		runErr := make(chan error, 1)
		eCode := make(chan int, 1)

		// Set current running step in emitter
		emitter.StartCmd(cmd)
		fmt.Fprintf(emitter, "$ %s\n", cmd.Cmd)

		fReader := bufio.NewReader(f)

		go func() {
			runCode, rcErr := doRunCommand(guid, stepFilePath, emitter, f, fReader)
			// exit code & errors from doRunCommand
			eCode <- runCode
			runErr <- rcErr
		}()

		select {
		case cmdErr = <-runErr:
			if firstError == nil {
				firstError = cmdErr
			}
			code = <-eCode
		case buildTimeout := <-invokeTimeout:
			handleBuildTimeout(f, buildTimeout)
			if firstError == nil {
				firstError = buildTimeout
				code = 3
			}
			_ = c.Process.Signal(syscall.SIGABRT)
			terminateSleep(shellBin, sourceDir, true) // kill all running sleep

		case stepAbort := <-sig:
			f.Write([]byte{4})
			if firstError == nil {
				firstError = stepAbort
				code = 1
			}
			_ = c.Process.Signal(syscall.SIGABRT)
			terminateSleep(shellBin, sourceDir, false) // kill all running sleep other than sleep $SD_TERMINATION_GRACE_PERIOD_SECS
		}

		if err := api.UpdateStepStop(buildID, cmd.Name, code); err != nil {
			return fmt.Errorf("Updating step stop %q: %v", cmd.Name, err)
		}
	}

	stepExitCode = code

	teardownCommands := append(userTeardownCommands, sdTeardownCommands...)

	for index, cmd := range teardownCommands {
		if index == 0 && firstError == nil {
			// Exit shell only if previous user steps ran successfully
			f.Write([]byte{4})
		}

		if err := api.UpdateStepStart(buildID, cmd.Name); err != nil {
			return fmt.Errorf("Updating step start %q: %v", cmd.Name, err)
		}

		code, cmdErr = doRunTeardownCommand(cmd, emitter, shellBin, exportFile, sourceDir, stepExitCode)

		if code != ExitOk {
			stepExitCode = code
		}

		if err := api.UpdateStepStop(buildID, cmd.Name, code); err != nil {
			return fmt.Errorf("Updating step stop %q: %v", cmd.Name, err)
		}

		if firstError == nil {
			firstError = cmdErr
		}
	}
	terminateSleep(shellBin, sourceDir, true) // kill running sleep $SD_TERMINATION_GRACE_PERIOD_SECS
	return firstError
}

// terminate long running sleep process for abort, timeout, n after teardown steps
func terminateSleep(shellBin, sourceDir string, killAll bool) {
	var stdout, stderr bytes.Buffer
	shargs := []string{"-e", "-c"}
	cmdStr := "pids=$(ps -ef | grep '[s]leep' | awk '{print $2}'); pidcnt=$(echo $pids | wc -w); if [ $pidcnt -gt 1 ]; then kill $(echo $pids | awk '{$NF=\"\"}1'); else echo $pids; fi;"
	if killAll {
		cmdStr = "pids=$(ps -ef | grep '[s]leep' | awk '{print $2}'); if [ ! -z $pids ]; then kill $pids; else echo $pids; fi;"
	}
	shargs = append(shargs, cmdStr)
	c := exec.Command(shellBin, shargs...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	c.Dir = sourceDir
	err := c.Run()
	if err != nil || strings.TrimSpace(stderr.String()) != "" {
		fmt.Printf("error %v, %v, in terminating sleep", err, strings.TrimSpace(stderr.String()))
	}
}
