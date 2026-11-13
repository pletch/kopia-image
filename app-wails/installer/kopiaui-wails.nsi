; NSIS installer for the KopiaUI Wails prototype.
;
; Built by ../build-installer.sh, which passes:
;   /DVERSION=<display version>   e.g. 20261113.0.0-232fe245
;   /DVIVERSION=<a.b.c.d>         numeric file version
;   /DSRCDIR=<dir>                directory with KopiaUI-Wails.exe and kopia.exe
;   /DOUTFILE=<path>              installer output path
;
; Per-user install: no administrator rights needed. Repository configuration
; in %APPDATA%\kopia is shared with Electron KopiaUI and the Kopia CLI and is
; never removed.

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user

!include "MUI2.nsh"
!include "LogicLib.nsh"

!define APPNAME      "KopiaUI Wails"
!define DISPLAYNAME  "KopiaUI (Wails prototype)"
!define EXENAME      "KopiaUI-Wails.exe"
!define COMPANY      "Kopia"
!define UNINSTKEY    "Software\Microsoft\Windows\CurrentVersion\Uninstall\KopiaUI-Wails"
!define RUNKEY       "Software\Microsoft\Windows\CurrentVersion\Run"
!define AUTOSTARTVAL "kopiaui-wails"
!define TOASTKEY     "Software\Classes\AppUserModelId\${APPNAME}"

Name "${DISPLAYNAME}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\KopiaUI-Wails"
InstallDirRegKey HKCU "${UNINSTKEY}" "InstallLocation"

VIProductVersion "${VIVERSION}"
VIAddVersionKey "ProductName" "${DISPLAYNAME}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "${DISPLAYNAME} installer"
VIAddVersionKey "CompanyName" "${COMPANY}"
VIAddVersionKey "LegalCopyright" "Apache License 2.0"

!define MUI_ICON   "..\icons\app.ico"
!define MUI_UNICON "..\icons\app.ico"
!define MUI_ABORTWARNING

!define MUI_WELCOMEPAGE_TEXT "This will install ${DISPLAYNAME} ${VERSION}.$\r$\n$\r$\nIt uses the same repositories as Electron KopiaUI. Do not run both at the same time: quit Electron KopiaUI from its tray icon before starting this one.$\r$\n$\r$\nClick Next to continue."
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXENAME}"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${DISPLAYNAME}"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; Asks a running instance to quit (it stops its Kopia servers cleanly), then
; force-kills it if it is still running after 10 seconds.
!macro StopRunningApp UN
Function ${UN}StopRunningApp
  IfFileExists "$INSTDIR\${EXENAME}" 0 wait
    DetailPrint "Closing running ${DISPLAYNAME}..."
    nsExec::Exec '"$INSTDIR\${EXENAME}" --quit'
    Pop $0
  wait:
  StrCpy $1 0
  ${Do}
    ; exit code 0 means the process is still running
    nsExec::Exec 'cmd /c tasklist /FI "IMAGENAME eq ${EXENAME}" /NH | find /I "${EXENAME}"'
    Pop $0
    ${If} $0 != 0
      ${Break}
    ${EndIf}
    ${If} $1 >= 20
      DetailPrint "Forcing ${EXENAME} to exit"
      nsExec::Exec 'taskkill /F /IM ${EXENAME}'
      Pop $0
      Sleep 1000
      ${Break}
    ${EndIf}
    Sleep 500
    IntOp $1 $1 + 1
  ${Loop}
FunctionEnd
!macroend

!insertmacro StopRunningApp ""
!insertmacro StopRunningApp "un."

Section "Install"
  SetShellVarContext current
  Call StopRunningApp

  SetOutPath "$INSTDIR"
  File "${SRCDIR}\${EXENAME}"
  File "${SRCDIR}\kopia.exe"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  CreateShortCut "$SMPROGRAMS\${DISPLAYNAME}.lnk" "$INSTDIR\${EXENAME}" "" "$INSTDIR\${EXENAME}" 0

  WriteRegStr HKCU "${UNINSTKEY}" "DisplayName" "${DISPLAYNAME}"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTKEY}" "Publisher" "${COMPANY}"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXENAME},0"
  WriteRegStr HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKCU "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "EstimatedSize" ${ESTIMATEDSIZEKB}
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  Call un.StopRunningApp

  Delete "$INSTDIR\${EXENAME}"
  Delete "$INSTDIR\kopia.exe"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${DISPLAYNAME}.lnk"

  ; launch-at-startup entry created by the app
  DeleteRegValue HKCU "${RUNKEY}" "${AUTOSTARTVAL}"

  ; toast notification registration created by the app (COM activator + AUMID)
  ReadRegStr $0 HKCU "${TOASTKEY}" "CustomActivator"
  ${If} $0 != ""
    DeleteRegKey HKCU "Software\Classes\CLSID\$0"
  ${EndIf}
  DeleteRegKey HKCU "${TOASTKEY}"

  ; WebView2 profile and app-specific files; shared Kopia configuration stays.
  RMDir /r "$LOCALAPPDATA\KopiaUI-Wails"
  Delete "$APPDATA\kopia\kopiaui-wails.log"
  Delete "$APPDATA\kopia\kopiaui-wails.log.1"
  Delete "$APPDATA\kopia\kopiaui-wails-windows.json"

  DeleteRegKey HKCU "${UNINSTKEY}"
SectionEnd
