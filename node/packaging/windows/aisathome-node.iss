; Installer for the AIs@home node on Windows (Inno Setup 6): the agent as a per-machine
; service under its own virtual account, its data in %ProgramData%\AIsAtHome, and the tray
; icon (aisathome-tray.exe) started at every user's login. Built by build-installer.ps1, which
; passes:
;   /DAppVersion=<version> /DFileVersion=<n.n.n> /DNodeExe=<aisathome-node.exe>
;   /DTrayExe=<aisathome-tray.exe> /DOutDir=<dir>
;   [/DEngineDir=<folder with llama-server.exe and its DLLs>]
; Silent install: setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART. Silent uninstall:
; unins000.exe /VERYSILENT /SUPPRESSMSGBOXES, which keeps the data folder; add /PURGEDATA to
; delete it too. Setup exits with 10 when the files are installed but the service could not be
; set up or started (the log says why).
;
; SPDX-License-Identifier: Apache-2.0

#ifndef AppVersion
  #error Define AppVersion: ISCC /DAppVersion=...
#endif
#ifndef NodeExe
  #error Define NodeExe, the path of the agent's executable: ISCC /DNodeExe=...
#endif
#ifndef TrayExe
  #error Define TrayExe, the path of the tray's executable: ISCC /DTrayExe=...
#endif
#ifndef FileVersion
  #define FileVersion "0.0.0"
#endif
#ifndef OutDir
  #define OutDir "."
#endif
#define TrayRunValue "AIs@home tray"

[Setup]
AppId={{5CA38A42-BDFD-4ADC-B507-2DC7D0B67B9F}
AppName=AIs@home node
AppVersion={#AppVersion}
AppVerName=AIs@home node {#AppVersion}
AppPublisher=AIs@home
AppPublisherURL=https://aisathome.org/
AppSupportURL=https://aisathome.org/
DefaultDirName={autopf}\AIsAtHome
DisableProgramGroupPage=yes
; Per machine: a service, Program Files and the data folder's permissions need administrator
; rights (one UAC prompt).
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir={#OutDir}
OutputBaseFilename=AIsAtHome-node-setup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
UninstallDisplayName=AIs@home node
UninstallDisplayIcon={app}\aisathome.ico
SetupIconFile=aisathome.ico
VersionInfoVersion={#FileVersion}
VersionInfoProductVersion={#FileVersion}
SetupLogging=yes

[Files]
Source: "{#NodeExe}"; DestDir: "{app}"; DestName: "aisathome-node.exe"; Flags: ignoreversion
Source: "{#TrayExe}"; DestDir: "{app}"; DestName: "aisathome-tray.exe"; Flags: ignoreversion
Source: "aisathome.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "THIRD_PARTY_NOTICES.md"; DestDir: "{app}"; Flags: ignoreversion
; The engine: the pinned upstream llama.cpp release (llama-server.exe, its DLLs and the CUDA
; runtime), unpacked by fetch-llama; release builds pass EngineDir, CI's installer test does not.
; The agent runs {app}\engine\llama-server.exe (node.WindowsLayout).
#ifdef EngineDir
Source: "{#EngineDir}\*"; DestDir: "{app}\engine"; Flags: ignoreversion recursesubdirs createallsubdirs
#endif

[InstallDelete]
; Before the tray (v0.4.x): the Start-menu entry was an Internet shortcut.
Type: files; Name: "{autoprograms}\AIs@home node.url"

[Registry]
; The tray starts at every user's login (per user, unelevated); Task Manager's Startup tab can
; turn it off.
Root: HKLM; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "{#TrayRunValue}"; ValueData: """{app}\aisathome-tray.exe"""; Flags: uninsdeletevalue

[Icons]
; The node's own page (status, and setup while it has no token), opened signed in by the tray
; (which also starts it when it is not running).
Name: "{autoprograms}\AIs@home node"; Filename: "{app}\aisathome-tray.exe"; Parameters: "-open"; IconFilename: "{app}\aisathome.ico"; Comment: "The AIs@home node's page"

[Run]
; The tray for the person who ran Setup, now rather than at the next login; then the page,
; signed in (a second tray start opens the page and exits).
Filename: "{app}\aisathome-tray.exe"; Flags: nowait runasoriginaluser skipifsilent
Filename: "{app}\aisathome-tray.exe"; Parameters: "-open"; Description: "Open the node's page"; Flags: postinstall nowait runasoriginaluser skipifsilent

[Code]
var
  ServiceFailed: Boolean;

{ Runs the agent's executable with Params, hidden, and copies its output into the log. }
function RunNode(const Exe, Params: String): Integer;
var
  OutFile: String;
  Lines: TArrayOfString;
  I, Code: Integer;
begin
  OutFile := ExpandConstant('{tmp}\aisathome-node-output.txt');
  DeleteFile(OutFile);
  { cmd /S /C strips the outer quotes: "exe" params > "file" 2>&1 }
  if not Exec(ExpandConstant('{cmd}'), '/S /C ""' + Exe + '" ' + Params + ' > "' + OutFile + '" 2>&1"',
      '', SW_HIDE, ewWaitUntilTerminated, Code) then
    Code := -1;
  Result := Code;
  if LoadStringsFromFile(OutFile, Lines) then
    for I := 0 to GetArrayLength(Lines) - 1 do
      Log('aisathome-node ' + Params + ': ' + Lines[I]);
  Log('aisathome-node ' + Params + ': exit code ' + IntToStr(Result));
end;

{ Closes every running tray (all users) before its file is replaced or removed: the trays in
  this session quit when asked, the others are ended. Not fatal: a tray left running only means
  Setup asks to close it or to restart. }
procedure QuitTrays(const Exe: String);
var
  Code: Integer;
begin
  if not FileExists(Exe) then
    Exit;
  if not Exec(Exe, '-quit', '', SW_HIDE, ewWaitUntilTerminated, Code) then
    Code := -1;
  Log('aisathome-tray -quit: exit code ' + IntToStr(Code));
end;

{ An update: close the trays and stop the running service with the new programs before their
  files are replaced. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  Result := '';
  ExtractTemporaryFile('aisathome-tray.exe');
  QuitTrays(ExpandConstant('{tmp}\aisathome-tray.exe'));
  ExtractTemporaryFile('aisathome-node.exe');
  Code := RunNode(ExpandConstant('{tmp}\aisathome-node.exe'), 'service stop');
  if Code <> 0 then
    Result := 'The running AIs@home node service could not be stopped (exit code ' + IntToStr(Code) +
      '). Stop it in the Services window, then run Setup again.';
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Code: Integer;
begin
  if CurStep = ssPostInstall then
  begin
    Code := RunNode(ExpandConstant('{app}\aisathome-node.exe'), 'service install -start');
    if Code <> 0 then
    begin
      ServiceFailed := True;
      SuppressibleMsgBox('The AIs@home node service could not be set up or started (exit code ' +
        IntToStr(Code) + '). Setup''s log has the details.', mbError, MB_OK, IDOK);
    end;
  end;
end;

function GetCustomSetupExitCode: Integer;
begin
  Result := 0;
  if ServiceFailed then
    Result := 10;
end;

function HasParam(const Name: String): Boolean;
var
  I: Integer;
begin
  Result := False;
  for I := 1 to ParamCount do
    if CompareText(ParamStr(I), Name) = 0 then
      Result := True;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Code: Integer;
  Data: String;
begin
  if CurUninstallStep = usUninstall then
  begin
    { Before the files go: close the trays, stop and remove the service. }
    QuitTrays(ExpandConstant('{app}\aisathome-tray.exe'));
    Code := RunNode(ExpandConstant('{app}\aisathome-node.exe'), 'service uninstall');
    if Code <> 0 then
      SuppressibleMsgBox('The AIs@home node service could not be removed (exit code ' + IntToStr(Code) +
        '). Setup''s log has the details.', mbError, MB_OK, IDOK);
  end
  else if CurUninstallStep = usPostUninstall then
  begin
    Data := ExpandConstant('{commonappdata}\AIsAtHome');
    if DirExists(Data) then
    begin
      if UninstallSilent then
      begin
        if HasParam('/PURGEDATA') then
          DelTree(Data, True, True, True);
      end
      else if MsgBox('Also delete the node''s data in ' + Data + '?' + #13#10#13#10 +
          'It holds the downloaded model (about 14 GB) and the node''s token, settings and history. ' +
          'Choose No to keep them for a later install.', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES then
        DelTree(Data, True, True, True);
    end;
  end;
end;
