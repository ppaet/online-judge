package runner

import (
	"io/ioutil"
	"time"

	"github.com/criyle/go-sandbox/daemon"
	"github.com/criyle/go-sandbox/pkg/cgroup"
	"github.com/criyle/go-sandbox/pkg/pipe"
	stypes "github.com/criyle/go-sandbox/types"

	"github.com/criyle/go-judge/file"
	"github.com/criyle/go-judge/file/memfile"
	"github.com/criyle/go-judge/language"
	"github.com/criyle/go-judge/types"
)

const maxOutput = 4 << 20 // 4M
const cgroupPrefix = "go-judge"
const minCPUPercent = 40 // 40%
const checkIntervalMS = 50

var env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

func (r *Runner) run(done <-chan struct{}, task *types.RunTask) *types.RunTaskResult {
	t := language.TypeExec
	if task.Type == "compile" {
		t = language.TypeCompile
	}
	param := r.Language.Get(task.Language, t)

	// init input / output / error files
	inputFile, err := task.InputFile.Open()
	if err != nil {
		return errResult("failed to initialize input file")
	}
	defer inputFile.Close()

	outputPipe, err := pipe.NewBuffer(maxOutput)
	if err != nil {
		return errResult("failed to initialize output pipe")
	}
	defer outputPipe.W.Close()

	errorPipe, err := pipe.NewBuffer(maxOutput)
	if err != nil {
		return errResult("failed to initialize output pipe")
	}
	defer errorPipe.W.Close()

	// init cgroup
	cg, err := cgroup.NewCGroup(cgroupPrefix)
	if err != nil {
		return errResult("failed to initialize cgroup")
	}
	defer cg.Destroy()

	// get daemon runner
	m, err := r.pool.Get()
	if err != nil {
		return errResult("failed to get daemon instance")
	}
	defer r.pool.Put(m)

	// setup cgroup limits
	memoryLimit := param.MemoryLimit
	if task.MemoryLimit > 0 {
		memoryLimit = task.MemoryLimit
	}

	cg.SetMemoryLimitInBytes(memoryLimit << 10)
	cg.SetPidsMax(param.ProcLimit)

	// set running parameters
	execParam := daemon.ExecveParam{
		Args:     param.Args,
		Envv:     env,
		Fds:      []uintptr{inputFile.Fd(), outputPipe.W.Fd(), errorPipe.W.Fd()},
		SyncFunc: cg.AddProc,
	}

	// cancellable signal channel
	cancelC := newCancellableChannel()
	defer cancelC.cancel()

	// start the process
	rc, err := m.Execve(cancelC.Done, &execParam)
	if err != nil {
		return errResult("failed to start program")
	}

	// close write end at parent process to avoid reader waiting
	// duplicate closing error is silenced during defer
	outputPipe.W.Close()
	errorPipe.W.Close()

	// wait task done (check each interval)
	ticker := time.NewTicker(checkIntervalMS * time.Millisecond)
	defer ticker.Stop()

	timeLimit := time.Duration(param.TimeLimit) * time.Millisecond
	if task.TimeLimit > 0 {
		timeLimit = time.Duration(task.TimeLimit) * time.Millisecond
	}

	var lastCPUUsage uint64
	var totalTime time.Duration
	var rt stypes.Result
	var rtreceived bool
	lastCheckTime := time.Now()

	// wait task finish
loop:
	for {
		select {
		case now := <-ticker.C: // interval
			cpuUsage, err := cg.CpuacctUsage()
			if err != nil {
				return errResult("failed to get cgroup cpu usage")
			}

			cpuUsageDelta := time.Duration(cpuUsage - lastCPUUsage)
			timeDelta := now.Sub(lastCheckTime)

			totalTime += durationMax(cpuUsageDelta, timeDelta*minCPUPercent/100)
			if totalTime > timeLimit {
				break loop
			}

			lastCheckTime = now
			lastCPUUsage = cpuUsage

		case rt = <-rc: // returned
			rtreceived = true
			break loop

		case <-outputPipe.Done: // outputlimit exceeded
			break loop

		case <-errorPipe.Done: // outputlimit exceeded
			break loop
		}
	}

	// get result if did not received
	cancelC.cancel()
	if !rtreceived {
		rt = <-rc
	}

	// generate resource usage
	cpuUsage, err := cg.CpuacctUsage()
	if err != nil {
		return errResult("failed to read cgroup cpuusage")
	}
	memoryUsage, err := cg.MemoryMaxUsageInBytes()
	if err != nil {
		return errResult("failed to read cgroup memory")
	}

	// generate result status
	status := ""
	if totalTime > timeLimit {
		status = "TLE"
	}
	if memoryUsage > memoryLimit<<10 {
		status = "MLE"
	}
	if outputPipe.Buffer.Len() > maxOutput {
		status = "OLE"
	}
	if errorPipe.Buffer.Len() > maxOutput {
		status = "OLE"
	}
	if rt.Status != stypes.StatusNormal {
		status = rt.Status.String()
	}

	inputContent, _ := task.InputFile.Content()

	// If compile read compiled files
	var exec []file.File
	if task.Type == "compile" {
		for _, fn := range param.CompiledFileNames {
			f, err := m.Open(fn)
			if err != nil {
				return errResult(err.Error())
			}
			defer f.Close()

			c, err := ioutil.ReadAll(f)
			if err != nil {
				return errResult(err.Error())
			}
			exec = append(exec, memfile.New(fn, c))
		}
	}

	// TODO: diff

	return &types.RunTaskResult{
		Status:     status,
		Time:       cpuUsage / uint64(time.Millisecond),
		Memory:     memoryUsage >> 10,
		Input:      inputContent,
		UserOutput: outputPipe.Buffer.Bytes(),
		UserError:  errorPipe.Buffer.Bytes(),
		ExecFiles:  exec,
	}
}

func errResult(err string) *types.RunTaskResult {
	return &types.RunTaskResult{
		Status: "JGF",
		Error:  err,
	}
}

type cancellableChannel struct {
	Done     chan struct{}
	canceled bool
}

func newCancellableChannel() *cancellableChannel {
	return &cancellableChannel{
		Done: make(chan struct{}),
	}
}

func (c *cancellableChannel) cancel() {
	if !c.canceled {
		close(c.Done)
		c.canceled = true
	}
}

func durationMax(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
