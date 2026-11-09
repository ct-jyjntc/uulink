Unicode true
!include "LogicLib.nsh"
!include "x64.nsh"
!include "WinVer.nsh"
!include "FileFunc.nsh"
LoadLanguageFile "${NSISDIR}\Contrib\Language files\SimpChinese.nlf"

; This executable is the silent installation backend for the custom Go/WebView UI.
Name "UU Link"
OutFile "${OUTPUT}"
InstallDir "$LOCALAPPDATA\Programs\UU Link"
RequestExecutionLevel user
SilentInstall silent
SetFont "Microsoft YaHei UI" 9
Icon "${APP_ICON}"
UninstallIcon "${APP_ICON}"
VIProductVersion "0.3.6.1"
VIAddVersionKey "ProductName" "UU Link"
VIAddVersionKey "FileDescription" "UU Link installation backend"
VIAddVersionKey "FileVersion" "0.3.6.1"
VIAddVersionKey "LegalCopyright" "UU Link contributors"
SetCompressor /SOLID lzma
UninstPage uninstConfirm
UninstPage instfiles

!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\UULink"
!define WV_RUNTIME "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"
Var StatusFile

Function .onInit
  ${IfNot} ${RunningX64}
  ${OrIfNot} ${AtLeastWin10}
    SetErrorLevel 3
    Quit
  ${EndIf}
  SetShellVarContext current
  SetRegView 64
  ${GetParameters} $0
  ${GetOptions} $0 "/STATUSFILE=" $StatusFile
FunctionEnd

Function Report
  Exch $8
  Push $9
  ${If} $StatusFile != ""
    ClearErrors
    FileOpen $9 "$StatusFile" w
    ${IfNot} ${Errors}
      FileWrite $9 "$8"
      FileClose $9
    ${EndIf}
  ${EndIf}
  Pop $9
  Pop $8
FunctionEnd

Function CheckRuntime
  StrCpy $1 0
  SetRegView 32
  ReadRegStr $0 HKLM "${WV_RUNTIME}" "pv"
  ${If} $0 != ""
  ${AndIf} $0 != "0.0.0.0"
    StrCpy $1 1
  ${Else}
    ReadRegStr $0 HKCU "${WV_RUNTIME}" "pv"
    ${If} $0 != ""
    ${AndIf} $0 != "0.0.0.0"
      StrCpy $1 1
    ${EndIf}
  ${EndIf}
  SetRegView 64
FunctionEnd

Section "UU Link"
  FindWindow $0 "UULinkDesktop"
  ${If} $0 != 0
    SetErrorLevel 2
    Quit
  ${EndIf}
  Push "runtime"
  Call Report
  Call CheckRuntime
  ${If} $1 == 0
    InitPluginsDir
    SetOutPath "$PLUGINSDIR"
    File "/oname=MicrosoftEdgeWebview2Setup.exe" "${BOOTSTRAPPER}"
    ExecWait '"$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" /silent /install' $0
    Call CheckRuntime
    ${If} $1 == 0
      SetErrorLevel 4
      Quit
    ${EndIf}
  ${EndIf}
  Push "files"
  Call Report
  ClearErrors
  SetOutPath "$INSTDIR"
  SetOverwrite on
  File "${STAGE}/UU Link.exe"
  File "${STAGE}/uulink-engine.exe"
  File "${STAGE}/THIRD_PARTY_NOTICES.txt"
  File "${STAGE}/README.txt"
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  ${If} ${Errors}
    SetErrorLevel 1
    Quit
  ${EndIf}
  Push "shortcuts"
  Call Report
  ClearErrors
  CreateShortCut "$SMPROGRAMS\UU Link.lnk" "$INSTDIR\UU Link.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "UU Link"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "0.3.6"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\UU Link.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
  ${If} ${Errors}
    SetErrorLevel 1
    Quit
  ${EndIf}
  Push "done"
  Call Report
  SetErrorLevel 0
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  SetRegView 64
  FindWindow $0 "UULinkDesktop"
  ${If} $0 != 0
    MessageBox MB_ICONSTOP "请从系统托盘菜单退出 UU Link，再继续卸载。" /SD IDOK
    SetErrorLevel 2
    Quit
  ${EndIf}
  ReadRegStr $0 HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "UULink"
  ${If} $0 == '"$INSTDIR\UU Link.exe"'
    DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "UULink"
  ${EndIf}
  Delete "$SMPROGRAMS\UU Link.lnk"
  Delete "$INSTDIR\UU Link.exe"
  Delete "$INSTDIR\uulink-engine.exe"
  Delete "$INSTDIR\README.txt"
  Delete "$INSTDIR\THIRD_PARTY_NOTICES.txt"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
SectionEnd
