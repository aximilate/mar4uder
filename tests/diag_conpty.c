#define _WIN32_WINNT 0x0A00
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>

#ifndef PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE
#define PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE 0x00020016
#endif

typedef void* HPCON;
typedef HRESULT (WINAPI *PFN_CreatePseudoConsole)(COORD, HANDLE, HANDLE, DWORD, HPCON*);
typedef HRESULT (WINAPI *PFN_ResizePseudoConsole)(HPCON, COORD);
typedef void (WINAPI *PFN_ClosePseudoConsole)(HPCON);

int main(void) {
    HMODULE hKernel32 = GetModuleHandleA("kernel32.dll");
    if (!hKernel32) {
        printf("GetModuleHandleA failed: %lu\n", GetLastError());
        return 1;
    }

    PFN_CreatePseudoConsole pfnCreatePseudoConsole = (PFN_CreatePseudoConsole)(void*)GetProcAddress(hKernel32, "CreatePseudoConsole");
    PFN_ClosePseudoConsole pfnClosePseudoConsole = (PFN_ClosePseudoConsole)(void*)GetProcAddress(hKernel32, "ClosePseudoConsole");

    if (!pfnCreatePseudoConsole) {
        printf("CreatePseudoConsole NOT found in kernel32.dll\n");
        return 1;
    }
    printf("[+] CreatePseudoConsole found!\n");

    HANDLE hPipeInRead, hPipeInWrite, hPipeOutRead, hPipeOutWrite;
    CreatePipe(&hPipeInRead, &hPipeInWrite, NULL, 0);
    CreatePipe(&hPipeOutRead, &hPipeOutWrite, NULL, 0);

    COORD size = {80, 24};
    HPCON hPC = NULL;
    HRESULT hr = pfnCreatePseudoConsole(size, hPipeInRead, hPipeOutWrite, 0, &hPC);
    if (FAILED(hr)) {
        printf("CreatePseudoConsole failed: 0x%08lx\n", hr);
        return 1;
    }
    printf("[+] CreatePseudoConsole succeeded!\n");

    CloseHandle(hPipeInRead);
    CloseHandle(hPipeOutWrite);

    STARTUPINFOEXA siEx;
    memset(&siEx, 0, sizeof(siEx));
    siEx.StartupInfo.cb = sizeof(STARTUPINFOEXA);

    SIZE_T bytesRequired = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &bytesRequired);
    siEx.lpAttributeList = (PPROC_THREAD_ATTRIBUTE_LIST)malloc(bytesRequired);
    if (!InitializeProcThreadAttributeList(siEx.lpAttributeList, 1, 0, &bytesRequired)) {
        printf("InitializeProcThreadAttributeList failed: %lu\n", GetLastError());
        return 1;
    }

    if (!UpdateProcThreadAttribute(siEx.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
                                   hPC, sizeof(HPCON), NULL, NULL)) {
        printf("UpdateProcThreadAttribute failed: %lu\n", GetLastError());
        return 1;
    }

    char cmd[] = "cmd.exe /c echo CONPTY_DIAG_SUCCESS";
    PROCESS_INFORMATION pi;
    memset(&pi, 0, sizeof(pi));

    if (!CreateProcessA(NULL, cmd, NULL, NULL, FALSE,
                        EXTENDED_STARTUPINFO_PRESENT, NULL, NULL,
                        &siEx.StartupInfo, &pi)) {
        printf("CreateProcessA failed: %lu\n", GetLastError());
        return 1;
    }
    printf("[+] CreateProcessA succeeded! PID: %lu\n", pi.dwProcessId);

    char buf[1024];
    DWORD bytesRead = 0;
    if (ReadFile(hPipeOutRead, buf, sizeof(buf) - 1, &bytesRead, NULL)) {
        buf[bytesRead] = 0;
        printf("[+] Output from ConPTY (%lu bytes):\n%s\n", bytesRead, buf);
    } else {
        printf("ReadFile failed: %lu\n", GetLastError());
    }

    pfnClosePseudoConsole(hPC);
    CloseHandle(hPipeInWrite);
    CloseHandle(hPipeOutRead);
    CloseHandle(pi.hProcess);
    CloseHandle(pi.hThread);
    DeleteProcThreadAttributeList(siEx.lpAttributeList);
    free(siEx.lpAttributeList);

    return 0;
}
