package edgepool

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func configureKernelProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

var kernelProcessJob struct {
	once   sync.Once
	handle windows.Handle
	err    error
}

// Put the owner into its private job BEFORE spawning a kernel. Windows then
// assigns every child (and descendant) to the job atomically during creation.
// Assigning only the child after cmd.Start would leave an orphan-producing gap
// if the owner died between process creation and AssignProcessToJobObject.
//
// The non-inheritable handle intentionally lives until the OWNER PROCESS exits.
// Do not close it in a pool or per-child cleanup: the owner is itself a member,
// so closing the last handle would terminate the owner as well. On an abrupt
// owner exit, Windows closes that handle and kills only this instance's tree.
// Existing enclosing jobs are supported by Windows nested-job semantics; an
// incompatible host job fails closed instead of starting an uncontained child.
func ensureKernelProcessJob() error {
	kernelProcessJob.once.Do(func() {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			kernelProcessJob.err = errors.New("kernel_job_unavailable")
			return
		}
		if err = windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			_ = windows.CloseHandle(job)
			kernelProcessJob.err = errors.New("kernel_job_unavailable")
			return
		}
		limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
		if err == nil {
			err = windows.AssignProcessToJobObject(job, windows.CurrentProcess())
		}
		if err != nil {
			_ = windows.CloseHandle(job)
			kernelProcessJob.err = errors.New("kernel_job_unavailable")
			return
		}
		kernelProcessJob.handle = job
	})
	return kernelProcessJob.err
}

func startKernelProcess(cmd *exec.Cmd) (func(), error) {
	// A changed parent or breakaway flag would defeat inherited job ownership.
	if cmd.SysProcAttr != nil && (cmd.SysProcAttr.ParentProcess != 0 || cmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB != 0) {
		return nil, errors.New("kernel_job_unavailable")
	}
	if err := ensureKernelProcessJob(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {}, nil // the owner's job handle must stay open for its lifetime
}
