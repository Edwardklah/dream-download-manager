; DreamDM.nsi - builds the Windows setup file, DreamDM-Setup-<version>.exe.
;
; Built by NSIS (nsis.sourceforge.io), from the project folder:
;     makensis /DVERSION=0.9.0 installer\windows\DreamDM.nsi
; (devloop's "app-installer" action does exactly this, after app-build.)
;
; What the setup does:
;   - asks whether to install for just this user (no admin needed, goes to
;     %LocalAppData%\Programs) or for everyone on the PC (Program Files);
;   - copies DreamDM.exe, dlm-host.exe (the browser extension's helper) and
;     the extension's files;
;   - tells Chrome and Edge where dlm-host is, so the extension can reach the
;     app (the same thing "dlm-host -install" does, but for the chosen users);
;   - adds a Start menu shortcut, optionally a desktop one, and an entry in
;     Settings > Apps so it can be uninstalled.
;
;
; Updates: the app's updater (gui/appupdate.go) runs this setup with
;     /S /CurrentUser (or /AllUsers) /RELAUNCH
; - no windows, the same kind of install as before, and the app opened again
; afterwards. The setup first waits for the app to close itself.
;
; The uninstaller undoes all of that. It never touches downloaded files, and
; only removes your settings and download list if you tick the box.

Unicode true
SetCompressor /SOLID lzma

!ifndef VERSION
  !define VERSION "0.0.0"
!endif

!define NAME      "Dream Download Manager"
!define EXE       "DreamDM.exe"
!define PUBLISHER "KLAH"
!define HOSTNAME  "com.dlmanager.host"            ; cmd/dlm-host: HostName
!define EXTID     "hjchnlipjbjlmobfaekngdjdjldjhpdk" ; cmd/dlm-host: ExtensionID
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\DreamDM"
!define APPKEY    "Software\Dream Download Manager"
!define ROOT      "..\.."                          ; the project folder

Name "${NAME}"
OutFile "${ROOT}\dist\DreamDM-Setup-${VERSION}.exe"
BrandingText "${NAME} ${VERSION}"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName"     "${NAME}"
VIAddVersionKey "ProductVersion"  "${VERSION}"
VIAddVersionKey "FileVersion"     "${VERSION}"
VIAddVersionKey "FileDescription" "${NAME} setup"
VIAddVersionKey "CompanyName"     "${PUBLISHER}"
VIAddVersionKey "LegalCopyright"  "Copyright (C) 2026 KLAH. GPL v3 or later."

; ---- Just me, or everyone? (NSIS's MultiUser.nsh) --------------------------
; "Highest": an administrator gets the choice (and one Windows permission
; prompt when the setup starts); anyone else installs just for themselves.
!define MULTIUSER_EXECUTIONLEVEL Highest
!define MULTIUSER_MUI
!define MULTIUSER_INSTALLMODE_COMMANDLINE        ; /AllUsers or /CurrentUser
!define MULTIUSER_INSTALLMODE_DEFAULT_CURRENTUSER ; "Only for me" is ticked at first
!define MULTIUSER_INSTALLMODEPAGE_SHOWUSERNAME
!define MULTIUSER_USE_PROGRAMFILES64
!define MULTIUSER_INSTALLMODE_INSTDIR "${NAME}"
; Where an earlier install is remembered, so an update goes to the same place
; and the uninstaller knows which kind it is.
!define MULTIUSER_INSTALLMODE_INSTDIR_REGISTRY_KEY        "${APPKEY}"
!define MULTIUSER_INSTALLMODE_INSTDIR_REGISTRY_VALUENAME  "InstallDir"
!define MULTIUSER_INSTALLMODE_DEFAULT_REGISTRY_KEY        "${APPKEY}"
!define MULTIUSER_INSTALLMODE_DEFAULT_REGISTRY_VALUENAME  "InstallDir"
!include MultiUser.nsh
!include MUI2.nsh
!include LogicLib.nsh
!include FileFunc.nsh

; ---- Look ------------------------------------------------------------------
!define MUI_ICON   "${ROOT}\gui\build\windows\icon.ico"
!define MUI_UNICON "${ROOT}\gui\build\windows\icon.ico"
!define MUI_WELCOMEFINISHPAGE_BITMAP   "side.bmp"
!define MUI_UNWELCOMEFINISHPAGE_BITMAP "side.bmp"
!define MUI_ABORTWARNING

; ---- Pages -----------------------------------------------------------------
!define MUI_WELCOMEPAGE_TITLE "Welcome to ${NAME}"
!define MUI_WELCOMEPAGE_TEXT "This will install ${NAME} ${VERSION}: a free, open-source download manager with no ads.$\r$\n$\r$\nIf Dream DM is running, setup will close it first. Downloads in progress pick up where they left off.$\r$\n$\r$\nClick Next to continue."
!insertmacro MUI_PAGE_WELCOME

; The GPL isn't a contract you sign: you may use the program without
; accepting it. So the button says Next, not "I Agree".
!define MUI_LICENSEPAGE_BUTTON "$(^NextBtn)"
!define MUI_LICENSEPAGE_TEXT_BOTTOM "You don't need to accept this to use Dream DM. It only applies if you share or change the program."
!insertmacro MUI_PAGE_LICENSE "license.txt"

!define MULTIUSER_PAGE_CUSTOMFUNCTION_PRE SkipModePageForUpdate
!insertmacro MULTIUSER_PAGE_INSTALLMODE
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_TITLE "${NAME} is installed"
!define MUI_FINISHPAGE_TEXT "You'll find it in the Start menu.$\r$\n$\r$\nThe first time it opens, a short welcome shows you how to download, pick a look, and connect your browser."
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Open ${NAME} now"
!define MUI_FINISHPAGE_RUN_FUNCTION OpenApp
; The "show readme" box, used for the desktop shortcut instead.
!define MUI_FINISHPAGE_SHOWREADME
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Put a shortcut on the desktop"
!define MUI_FINISHPAGE_SHOWREADME_NOTCHECKED
!define MUI_FINISHPAGE_SHOWREADME_FUNCTION DesktopShortcut
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!define MUI_COMPONENTSPAGE_TEXT_TOP "Your downloaded files are never removed. Tick the box below to also remove your settings and download list."
!insertmacro MUI_UNPAGE_COMPONENTS
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; ---- Start -----------------------------------------------------------------
Var Relaunch ; 1: started by the app's updater (/RELAUNCH)

Function .onInit
  !insertmacro MULTIUSER_INIT
  ${GetParameters} $R0
  ClearErrors
  ${GetOptions} $R0 "/RELAUNCH" $R1
  ${IfNot} ${Errors}
    StrCpy $Relaunch 1
  ${EndIf}
FunctionEnd

Function un.onInit
  !insertmacro MULTIUSER_UNINIT
FunctionEnd

; An update goes where the old version is; don't ask again.
Function SkipModePageForUpdate
  ${If} $MultiUser.InstallMode == "AllUsers"
    ReadRegStr $0 HKLM "${APPKEY}" "InstallDir"
  ${Else}
    ReadRegStr $0 HKCU "${APPKEY}" "InstallDir"
  ${EndIf}
  ${If} $0 != ""
    Abort ; skips this page
  ${EndIf}
FunctionEnd

; Close the app and its helper, so their files can be replaced or removed.
!macro CloseApp
  nsExec::Exec 'taskkill /F /IM "${EXE}"'
  nsExec::Exec 'taskkill /F /IM "dlm-host.exe"'
  Sleep 500
!macroend

; ---- Install ---------------------------------------------------------------
Section "Install"
  ${If} $Relaunch == 1
    ; The app closes itself after starting this; give it up to 15 s to save
    ; its list and go, before CloseApp would force it.
    StrCpy $R2 0
    ${Do}
      ; find's exit code: 0 = still running, 1 = gone.
      nsExec::Exec 'cmd /c tasklist /FI "IMAGENAME eq ${EXE}" /NH | find /I "${EXE}"'
      Pop $R3
      ${IfThen} $R3 != 0 ${|} ${ExitDo} ${|}
      IntOp $R2 $R2 + 1
      ${IfThen} $R2 >= 30 ${|} ${ExitDo} ${|}
      Sleep 500
    ${Loop}
  ${EndIf}
  !insertmacro CloseApp

  SetOutPath "$INSTDIR"
  File "${ROOT}\gui\bin\${EXE}"
  File "${ROOT}\gui\bin\dlm-host.exe"
  File "/oname=LICENSE.txt" "license.txt"

  ; The browser extension, for "Load unpacked" (the welcome screen shows how).
  SetOutPath "$INSTDIR\extension"
  File /r "${ROOT}\extension\*.*"
  SetOutPath "$INSTDIR"

  ; Tell Chrome and Edge where the helper is. The manifest names the program
  ; and the one extension allowed to start it. (JSON needs \\ for each \.)
  StrCpy $0 "$INSTDIR\dlm-host.exe"
  Push $0
  Call JSONPath
  Pop $0
  FileOpen $1 "$INSTDIR\${HOSTNAME}.json" w
  FileWrite $1 '{$\r$\n'
  FileWrite $1 '  "name": "${HOSTNAME}",$\r$\n'
  FileWrite $1 '  "description": "Dream Download Manager - receives links from the browser extension",$\r$\n'
  FileWrite $1 '  "path": "$0",$\r$\n'
  FileWrite $1 '  "type": "stdio",$\r$\n'
  FileWrite $1 '  "allowed_origins": ["chrome-extension://${EXTID}/"]$\r$\n'
  FileWrite $1 '}$\r$\n'
  FileClose $1
  ; SHCTX = HKLM for everyone, HKCU for just this user (set by MultiUser).
  WriteRegStr SHCTX "Software\Google\Chrome\NativeMessagingHosts\${HOSTNAME}" "" "$INSTDIR\${HOSTNAME}.json"
  WriteRegStr SHCTX "Software\Microsoft\Edge\NativeMessagingHosts\${HOSTNAME}" "" "$INSTDIR\${HOSTNAME}.json"

  CreateShortcut "$SMPROGRAMS\${NAME}.lnk" "$INSTDIR\${EXE}"

  ; Settings > Apps entry.
  WriteUninstaller "$INSTDIR\uninstall.exe"
  WriteRegStr   SHCTX "${APPKEY}" "InstallDir" "$INSTDIR"
  WriteRegStr   SHCTX "${UNINSTKEY}" "DisplayName" "${NAME}"
  WriteRegStr   SHCTX "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr   SHCTX "${UNINSTKEY}" "Publisher" "${PUBLISHER}"
  WriteRegStr   SHCTX "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr   SHCTX "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr   SHCTX "${UNINSTKEY}" "UninstallString" '"$INSTDIR\uninstall.exe" /$MultiUser.InstallMode'
  WriteRegStr   SHCTX "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /$MultiUser.InstallMode /S'
  WriteRegDWORD SHCTX "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD SHCTX "${UNINSTKEY}" "NoRepair" 1
  ; Size in KB, for Settings > Apps.
  SectionGetSize 0 $0
  WriteRegDWORD SHCTX "${UNINSTKEY}" "EstimatedSize" $0

  ; An update from the app: open it again.
  ${If} $Relaunch == 1
    Call OpenApp
  ${EndIf}
SectionEnd

; JSONPath: doubles every backslash in the string on the stack.
Function JSONPath
  Exch $0 ; input
  Push $1 ; output
  Push $2 ; index
  Push $3 ; character
  StrCpy $1 ""
  StrCpy $2 0
  ${Do}
    StrCpy $3 $0 1 $2
    ${IfThen} $3 == "" ${|} ${ExitDo} ${|}
    ${If} $3 == "\"
      StrCpy $1 "$1\\"
    ${Else}
      StrCpy $1 "$1$3"
    ${EndIf}
    IntOp $2 $2 + 1
  ${Loop}
  StrCpy $0 $1
  Pop $3
  Pop $2
  Pop $1
  Exch $0
FunctionEnd

; Open the app as the signed-in user, not as administrator (the setup may be
; running as administrator; the app shouldn't). Explorer starts it for us.
Function OpenApp
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\${EXE}"'
FunctionEnd

Function DesktopShortcut
  CreateShortcut "$DESKTOP\${NAME}.lnk" "$INSTDIR\${EXE}"
FunctionEnd

; ---- Uninstall -------------------------------------------------------------
Section "un.Dream Download Manager" UNSEC_APP
  SectionIn RO
  !insertmacro CloseApp

  DeleteRegKey SHCTX "Software\Google\Chrome\NativeMessagingHosts\${HOSTNAME}"
  DeleteRegKey SHCTX "Software\Microsoft\Edge\NativeMessagingHosts\${HOSTNAME}"
  DeleteRegKey SHCTX "${UNINSTKEY}"
  DeleteRegKey SHCTX "${APPKEY}"
  ; "Start with Windows" (gui/system_windows.go: runValue), for this user.
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "dlmanager"

  Delete "$SMPROGRAMS\${NAME}.lnk"
  Delete "$DESKTOP\${NAME}.lnk"

  ; Only the files setup put here - never the whole folder blindly.
  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\dlm-host.exe"
  Delete "$INSTDIR\${HOSTNAME}.json"
  Delete "$INSTDIR\LICENSE.txt"
  RMDir /r "$INSTDIR\extension"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR" ; only if now empty
SectionEnd

; Settings, download list, remembered servers and yt-dlp live in
; %AppData%\dlmanager - the signed-in user's own folder.
Section /o "un.Also remove my settings and download list" UNSEC_DATA
  SetShellVarContext current
  RMDir /r "$APPDATA\dlmanager"
SectionEnd

!insertmacro MUI_UNFUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${UNSEC_APP}  "The program, its shortcuts and its link to your browser."
  !insertmacro MUI_DESCRIPTION_TEXT ${UNSEC_DATA} "Settings, the download list, remembered servers and the video tool (yt-dlp). Your downloaded files stay."
!insertmacro MUI_UNFUNCTION_DESCRIPTION_END
