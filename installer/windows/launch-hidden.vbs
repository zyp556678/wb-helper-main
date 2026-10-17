' Launch the gateway with NO visible console window, appending output to a log file.
'
' Why this file exists: the optional "start at logon" feature runs through Task
' Scheduler. A console application started that way pops up a black window at every
' logon, which is unacceptable for something that is supposed to run in the
' background. WScript.Shell.Run with window style 0 (hidden) is the standard way to
' start a console program invisibly.
'
' Because there is no window, the output must go somewhere: it is appended to
' %LOCALAPPDATA%\wb-gateway\logs\gateway.log. Without that, a background gateway that
' fails to start would be completely undiagnosable.
'
' Port and API key come from launch.txt next to this file (written by install.ps1).
' run.cmd reads the same file, so the shortcut and the logon task cannot disagree
' about either value. A port argument is still accepted for older scheduled tasks
' that were registered with the number baked in.
'
' Usage: wscript.exe launch-hidden.vbs [port]

Option Explicit

Dim sh, fso, baseDir, exePath, dataDir, logDir, logFile, port, apiKey, cmd, q
Dim wmi, procs, already, launchFile, stream, line, eq, key, value

Set sh = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

baseDir = fso.GetParentFolderName(WScript.ScriptFullName)
exePath = fso.BuildPath(baseDir, "workbuddy-gateway.exe")

If Not fso.FileExists(exePath) Then
    ' Nothing to report to: exit silently so the logon task does not error out
    WScript.Quit 1
End If

port = "8317"
apiKey = ""

launchFile = fso.BuildPath(baseDir, "launch.txt")
If fso.FileExists(launchFile) Then
    On Error Resume Next
    Set stream = fso.OpenTextFile(launchFile, 1, False)
    If Err.Number = 0 Then
        Do While Not stream.AtEndOfStream
            line = Trim(stream.ReadLine)
            eq = InStr(line, "=")
            If eq > 1 Then
                key = LCase(Trim(Left(line, eq - 1)))
                value = Trim(Mid(line, eq + 1))
                If key = "port" And Len(value) > 0 Then port = value
                If key = "api_key" Then apiKey = value
            End If
        Loop
        stream.Close
    End If
    On Error Goto 0
End If

' An explicit argument still wins: a scheduled task registered by an older
' installer carries the port on its command line.
If WScript.Arguments.Count > 0 Then
    If Len(Trim(WScript.Arguments(0))) > 0 Then port = Trim(WScript.Arguments(0))
End If

dataDir = sh.ExpandEnvironmentStrings("%LOCALAPPDATA%") & "\wb-gateway"
logDir = dataDir & "\logs"

If Not fso.FolderExists(dataDir) Then fso.CreateFolder(dataDir)
If Not fso.FolderExists(logDir) Then fso.CreateFolder(logDir)

' Do not start a second copy: the gateway would fail to bind the port and append a
' confusing error to the log on every logon.
already = False
On Error Resume Next
Set wmi = GetObject("winmgmts:\\.\root\cimv2")
Set procs = wmi.ExecQuery("SELECT Name FROM Win32_Process WHERE Name='workbuddy-gateway.exe'")
If Err.Number = 0 Then
    If procs.Count > 0 Then already = True
End If
On Error Goto 0

If already Then WScript.Quit 0

logFile = fso.BuildPath(logDir, "gateway.log")

' Build the command with Chr(34) for quotes: literal "" escaping inside a VBScript
' string is error prone (a path with spaces plus a redirect plus a quote is four
' levels of quoting), and this form reads exactly like the final command line.
q = Chr(34)
' Declare the install form so the updater does not replace this command-line
' install with the desktop package (see internal/update/update.go detectForm).
' The outer pair of quotes is stripped by `cmd /c`, leaving a plain
' `set VAR=cli&& "exe" serve ...` command line.
cmd = "cmd /c " & q & "set WB_GATEWAY_HOST=cli&& " & q & exePath & q & " serve --port " & port & " --auth-dir " & q & dataDir & q
If Len(apiKey) > 0 Then cmd = cmd & " --api-key " & q & apiKey & q
cmd = cmd & " >> " & q & logFile & q & " 2>&1" & q

' 0 = hidden window, False = do not wait
sh.Run cmd, 0, False
