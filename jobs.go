package main

import (
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Дочерние процессы — ядро, sing-box, клиент VK TURN — живут в job object с
// флагом «убить при закрытии». Когда olcvpn завершается любым способом,
// включая падение или «Снять задачу», Windows закрывает последний дескриптор
// job и сама добивает всех. Без этого sing-box с TUN переживал olcvpn, держал
// маршрут по умолчанию на мёртвом туннеле, и интернет пропадал до
// перезагрузки.
var (
	jobOnce sync.Once
	job     windows.Handle
)

func killOnExitJob() windows.Handle {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(h)
			return
		}
		// Дескриптор намеренно не закрываем: его закроет сама система при
		// выходе процесса, и это и есть сигнал убить детей.
		job = h
	})
	return job
}

// bindToApp makes p die together with olcvpn. Failure is not fatal: the
// child still works, it just would not be cleaned up after a crash.
func bindToApp(p *os.Process) {
	j := killOnExitJob()
	if j == 0 || p == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(j, h)
}
