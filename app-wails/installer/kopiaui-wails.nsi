; NSIS installer for KopiaUI-Wails.
;
; Built by ../build-installer.sh, which passes:
;   /DVERSION=<display version>   e.g. 20261113.0.0-232fe245
;   /DVIVERSION=<a.b.c.d>         numeric file version
;   /DSRCDIR=<dir>                directory with KopiaUI-Wails.exe and kopia.exe
;   /DOUTFILE=<path>              installer output path
;   /DWINFSPMSI=<path>            WinFsp MSI offered during install
;   /DWINFSPVERSION=<version>     its version
;
; Per-user install: no administrator rights needed. Repository configuration
; in %APPDATA%\kopia is shared with Electron KopiaUI and the Kopia CLI and is
; never removed.

Unicode true
SetCompressor /SOLID lzma
RequestExecutionLevel user

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WordFunc.nsh"

!define APPNAME      "KopiaUI Wails"
!define DISPLAYNAME  "KopiaUI-Wails"
; Start menu shortcut name used by builds before the rename.
!define OLDSHORTCUT  "KopiaUI (Wails prototype)"
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

; UpdateImageTasks offers to make the scheduled disk image tasks run the kopia
; just installed. The tasks run as SYSTEM a copy of kopia in a folder only
; administrators can change, since this per-user folder is not one, so the
; copy does not follow upgrades. kopia tells whether the tasks run another
; version, needing no administrator; updating them asks for one. Silent
; installs leave the tasks as they are.
Function UpdateImageTasks
  IfSilent done

  nsExec::ExecToStack '"$INSTDIR\kopia.exe" image schedule update --check'
  Pop $0
  Pop $1
  ${If} $0 != "0"
    Goto done
  ${EndIf}

  ; "outdated <tasks' version> <this version>"
  StrCpy $2 $1 8
  ${If} $2 != "outdated"
    Goto done
  ${EndIf}

  ${WordFind} $1 " " "+2" $3

  MessageBox MB_YESNO|MB_ICONQUESTION "Disk images of this computer are scheduled, and the scheduled tasks run kopia $3.$\r$\n$\r$\nUpdate them to this version now? Windows asks for administrator rights." IDNO done

  ExecShellWait "runas" "$INSTDIR\kopia.exe" "image schedule update" SW_HIDE

  nsExec::ExecToStack '"$INSTDIR\kopia.exe" image schedule update --check'
  Pop $0
  Pop $1
  StrCpy $2 $1 10
  ${If} $2 == "up-to-date"
    MessageBox MB_OK|MB_ICONINFORMATION "The scheduled image tasks now run this version of kopia."
  ${Else}
    MessageBox MB_OK|MB_ICONEXCLAMATION "The scheduled image tasks were not updated. Use Update the Tasks on the Images page of ${DISPLAYNAME}."
  ${EndIf}

done:
FunctionEnd

; InstalledWinFspVersion pushes the version of the installed WinFsp, such as
; "2.1.25156.0", read from its DLL, or "" when WinFsp is not installed. The
; registry key that WinFsp writes holds no version.
Function InstalledWinFspVersion
  Push $0
  Push $1
  Push $2
  Push $3
  Push $4
  Push $5

  StrCpy $5 ""

  ; the installer is 32-bit, so this reads the key WinFsp writes
  ReadRegStr $0 HKLM "SOFTWARE\WinFsp" "InstallDir"
  ${If} $0 != ""
    ClearErrors
    GetDLLVersion "$0\bin\winfsp-x64.dll" $1 $2
    ${IfNot} ${Errors}
      IntOp $3 $1 >> 16
      IntOp $3 $3 & 0xFFFF
      IntOp $4 $1 & 0xFFFF
      StrCpy $5 "$3.$4"
      IntOp $3 $2 >> 16
      IntOp $3 $3 & 0xFFFF
      IntOp $4 $2 & 0xFFFF
      StrCpy $5 "$5.$3.$4"
    ${EndIf}
  ${EndIf}

  Pop $4
  Pop $3
  Pop $2
  Pop $1
  Pop $0
  Exch $5
FunctionEnd

; OfferWinFsp offers to install WinFsp, or to update an older one, with which
; kopia mounts snapshots as native drives; without it they are WebDAV
; drives, which Windows limits to files of 50 MB and reads slowly. WinFsp
; installs for all users, so it asks for administrator rights. Silent
; installs leave it as it is.
Function OfferWinFsp
  IfSilent done

  Call InstalledWinFspVersion
  Pop $0

  ${If} $0 == ""
    MessageBox MB_YESNO|MB_ICONQUESTION "Install WinFsp ${WINFSPVERSION} too?$\r$\n$\r$\nWith WinFsp, mounted snapshots are native drives: files of any size can be opened and copied, and browsing is faster. Without it they are WebDAV drives, which Windows limits to files of 50 MB.$\r$\n$\r$\nWinFsp (winfsp.dev) is free software installed for all users. Windows asks for administrator rights." IDNO done
  ${Else}
    ; 0: same version, 1: the installed one is newer, 2: it is older
    ${VersionCompare} $0 "${WINFSPVERSION}.0" $1
    ${If} $1 != 2
      Goto done
    ${EndIf}

    MessageBox MB_YESNO|MB_ICONQUESTION "Update WinFsp $0 to ${WINFSPVERSION}?$\r$\n$\r$\nKopia mounts snapshots as native drives with WinFsp, and is tested with this version. Unmount any mounted snapshots first; Windows may need a restart to finish the update.$\r$\n$\r$\nWindows asks for administrator rights." IDNO done
  ${EndIf}

  InitPluginsDir
  File "/oname=$PLUGINSDIR\winfsp.msi" "${WINFSPMSI}"
  DetailPrint "Installing WinFsp ${WINFSPVERSION}..."
  ExecShellWait "runas" "msiexec.exe" '/i "$PLUGINSDIR\winfsp.msi" /qb /norestart'

  Call InstalledWinFspVersion
  Pop $1

  ${If} $1 == ""
    MessageBox MB_OK|MB_ICONEXCLAMATION "WinFsp was not installed. Snapshots will be mounted as WebDAV drives. You can install WinFsp from winfsp.dev later."
  ${Else}
    ${VersionCompare} $1 "${WINFSPVERSION}.0" $2
    ${If} $2 == 2
      MessageBox MB_OK|MB_ICONEXCLAMATION "WinFsp was not updated: version $1 is installed. Snapshots are still mounted with it."
    ${EndIf}
  ${EndIf}

done:
FunctionEnd

Section "Install"
  SetShellVarContext current
  Call StopRunningApp

  SetOutPath "$INSTDIR"
  File "${SRCDIR}\${EXENAME}"
  File "${SRCDIR}\kopia.exe"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  Delete "$SMPROGRAMS\${OLDSHORTCUT}.lnk"
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

  Call UpdateImageTasks
  Call OfferWinFsp
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  Call un.StopRunningApp

  Delete "$INSTDIR\${EXENAME}"
  Delete "$INSTDIR\kopia.exe"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"

  Delete "$SMPROGRAMS\${DISPLAYNAME}.lnk"
  Delete "$SMPROGRAMS\${OLDSHORTCUT}.lnk"

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
  Delete "$APPDATA\kopia\kopiaui-wails-state.json"

  DeleteRegKey HKCU "${UNINSTKEY}"
SectionEnd
