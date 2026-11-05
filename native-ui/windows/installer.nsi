Unicode true
!include "LogicLib.nsh"
!include "x64.nsh"
!include "WinVer.nsh"
!include "FileFunc.nsh"

Name "UU Link"
OutFile "${OUTPUT}"
InstallDir "$LOCALAPPDATA\Programs\UU Link"
RequestExecutionLevel user
Icon "${APP_ICON}"
UninstallIcon "${APP_ICON}"
VIProductVersion "0.3.5.1"
VIAddVersionKey "ProductName" "UU Link"
VIAddVersionKey "FileDescription" "UU Link Setup"
VIAddVersionKey "FileVersion" "0.3.5.1"
VIAddVersionKey "LegalCopyright" "UU Link contributors"
SetCompressor /SOLID lzma
ShowInstDetails show

!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\UULink"
!define WV_RUNTIME "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

Function .onInit
  ${IfNot} ${RunningX64}
  ${OrIfNot} ${AtLeastWin10}
    MessageBox MB_ICONSTOP "UU Link requires Windows 10 or 11 (x64)."
    Abort
  ${EndIf}
  SetShellVarContext current
  SetRegView 64
  ReadRegStr $0 HKCU "${UNINSTALL_KEY}" "InstallLocation"
  ${If} $0 != ""
    StrCpy $INSTDIR $0
  ${EndIf}
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
    MessageBox MB_ICONSTOP "Close UU Link before updating it."
    Abort
  ${EndIf}
  Call CheckRuntime
  ${If} $1 == 0
    InitPluginsDir
    SetOutPath "$PLUGINSDIR"
    File "/oname=MicrosoftEdgeWebview2Setup.exe" "${BOOTSTRAPPER}"
    ExecWait '"$PLUGINSDIR\MicrosoftEdgeWebview2Setup.exe" /silent /install' $0
    Call CheckRuntime
    ${If} $1 == 0
      MessageBox MB_ICONSTOP "Microsoft Edge WebView2 Runtime is required. Install it from Microsoft and run this setup again."
      Abort
    ${EndIf}
  ${EndIf}

  SetOutPath "$INSTDIR"
  SetOverwrite on
  File "${STAGE}/UU Link.exe"
  File "${STAGE}/uulink-engine.exe"
  File "${STAGE}/THIRD_PARTY_NOTICES.txt"
  File "${STAGE}/README.txt"
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  CreateShortCut "$SMPROGRAMS\UU Link.lnk" "$INSTDIR\UU Link.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName" "UU Link"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion" "0.3.5"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon" "$INSTDIR\UU Link.exe"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  SetRegView 64
  FindWindow $0 "UULinkDesktop"
  ${If} $0 != 0
    MessageBox MB_ICONSTOP "Close UU Link before uninstalling it."
    Abort
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
