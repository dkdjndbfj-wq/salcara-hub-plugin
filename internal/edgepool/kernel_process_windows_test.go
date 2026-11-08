package edgepool

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type kernelHelperInfo struct {
	Child      int    `json:"child"`
	Grandchild int    `json:"grandchild"`
	JobFlags   uint32 `json:"job_flags"`
	Error      string `json:"error"`
}

func kernelWindowsHelperCommand(mode, dir string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestKernelWindowsProcessHelper$", "--", mode, dir)
	configureKernelProcess(cmd)
	return cmd
}

// This helper executes only in a child test process. In particular the test
// runner never joins the KillOnJobClose job created by the owner helper.
func TestKernelWindowsProcessHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 || len(os.Args) != index+3 {
		return
	}
	mode, dir := os.Args[index+1], os.Args[index+2]
	write := func(file string, value kernelHelperInfo) {
		raw, _ := json.Marshal(value)
		if os.WriteFile(filepath.Join(dir, file), raw, 0600) != nil {
			os.Exit(3)
		}
	}
	switch mode {
	case "owner":
		cmd := kernelWindowsHelperCommand("child", dir)
		cleanup, err := startKernelProcess(cmd)
		if err != nil {
			write("owner.json", kernelHelperInfo{Error: err.Error()})
			os.Exit(2)
		}
		var flags uint32
		getFlags := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")
		ok, _, err := getFlags.Call(uintptr(kernelProcessJob.handle), uintptr(unsafe.Pointer(&flags)))
		if ok == 0 {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			write("owner.json", kernelHelperInfo{Error: err.Error()})
			os.Exit(2)
		}
		write("owner.json", kernelHelperInfo{Child: cmd.Process.Pid, JobFlags: flags})
		_ = cmd.Wait()
		cleanup()
	case "child":
		// Deliberately use ordinary exec here: descendants must inherit the job
		// atomically even without calling our managed-process wrapper themselves.
		grandchild := kernelWindowsHelperCommand("grandchild", dir)
		if err := grandchild.Start(); err != nil {
			write("child.json", kernelHelperInfo{Error: err.Error()})
			os.Exit(2)
		}
		write("child.json", kernelHelperInfo{Child: os.Getpid(), Grandchild: grandchild.Process.Pid})
		_ = grandchild.Wait()
	case "grandchild", "control":
		if err := os.WriteFile(filepath.Join(dir, mode+".ready"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Second)
		}
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func readKernelHelperInfo(t *testing.T, path string) kernelHelperInfo {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var info kernelHelperInfo
			if json.Unmarshal(raw, &info) == nil {
				if info.Error != "" {
					t.Fatal(info.Error)
				}
				return info
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("managed process helper did not become ready")
	return kernelHelperInfo{}
}

func openKernelTestProcess(t *testing.T, pid int) windows.Handle {
	t.Helper()
	if pid <= 0 {
		t.Fatal("invalid helper process ID")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h
}

func TestKernelWindowsOwnerDeathTerminatesOnlyOwnedTree(t *testing.T) {
	dir := t.TempDir()
	// An unrelated helper owned by this test must survive the other owner's job.
	control := kernelWindowsHelperCommand("control", dir)
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Process.Kill(); _ = control.Wait() })
	controlHandle := openKernelTestProcess(t, control.Process.Pid)
	owner := kernelWindowsHelperCommand("owner", dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerWaited := false
	t.Cleanup(func() {
		if !ownerWaited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	info := readKernelHelperInfo(t, filepath.Join(dir, "owner.json"))
	if info.JobFlags&windows.HANDLE_FLAG_INHERIT != 0 {
		t.Fatal("job handle can leak into the managed child")
	}
	child := readKernelHelperInfo(t, filepath.Join(dir, "child.json"))
	if child.Child != info.Child {
		t.Fatal("helper ownership mismatch")
	}
	childHandle := openKernelTestProcess(t, child.Child)
	grandchildHandle := openKernelTestProcess(t, child.Grandchild)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	ownerWaited = true
	for name, handle := range map[string]windows.Handle{"child": childHandle, "grandchild": grandchildHandle} {
		status, err := windows.WaitForSingleObject(handle, 5000)
		if err != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatalf("%s survived abrupt owner death: wait=%d err=%v", name, status, err)
		}
	}
	if status, err := windows.WaitForSingleObject(controlHandle, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("unrelated helper was terminated: wait=%d err=%v", status, err)
	}
}

func TestKernelWindowsRejectsBreakawayBeforeSpawn(t *testing.T) {
	cmd := kernelWindowsHelperCommand("control", t.TempDir())
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	if cleanup, err := startKernelProcess(cmd); err == nil || cleanup != nil || cmd.Process != nil {
		t.Fatal("breakaway child was allowed")
	}
	cmd = kernelWindowsHelperCommand("control", t.TempDir())
	cmd.SysProcAttr.ParentProcess = syscall.Handle(123)
	if cleanup, err := startKernelProcess(cmd); err == nil || cleanup != nil || cmd.Process != nil {
		t.Fatal("changed-parent child was allowed")
	}
}
