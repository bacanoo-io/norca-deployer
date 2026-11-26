# Windows Nodes

Windows nodes are supported as clients (typically for native `.exe` workloads with `raw_exec`) and as servers. Windows support is meant for optional installations and testing; production clusters should use Linux nodes.

## Requirements

- OpenSSH Server, reachable from the operator machine.
- An administrator account (`connection.become` is not supported on Windows; `validate` rejects it).
- `platform: windows` on the node.

## Differences from Linux

| Area | Windows behavior |
| --- | --- |
| Commands | Run as `powershell.exe -EncodedCommand`, because the default OpenSSH shell is `cmd.exe` |
| Host facts | Not probed: the facts come from the plan (`platform: windows`, family `windows`, no package manager); `host_facts` is refused |
| File transfer | SFTP paths in the form `/C:/...` |
| Binary update | The running `norca.exe` cannot be overwritten: the new binary is staged as `norca.exe.new` and swapped in while the service is stopped; a staged binary restarts the agent in the same `apply` |
| Service | `norca` service created with `New-Service`, quoted `ImagePath` (paths with spaces), automatic start, restart on failure |
| Firewall | Not configured: open the ports in Windows Firewall yourself (see [Networking and tuning](networking-and-tuning.md#firewall) for the ports) |
| Tuning | `tuning.windows_long_paths: true` sets `LongPathsEnabled = 1` and the execution policy `RemoteSigned` |
| Paths | Separate defaults under `C:\Program Files\norca` and `C:\ProgramData\norca` (see [Networking and tuning](networking-and-tuning.md)) |
| Host packages | No package manager: none of the blocks of `host_packages` applies; a selection for the node is skipped with a warning |
| Scripts | Run in Windows PowerShell 5.1 (`interpreter: powershell`, the default on Windows nodes); `bash` and `sh` scripts are skipped without a warning; see [Scripts](#scripts) |
| [GitOps](gitops.md) bindings on `apply` | Not run when the first server runs Windows: `apply` fails; `gitops run` works |
| CNI plugins | Not installed |
| `release.source: host-package` | Not possible; `validate` rejects it |

## Example

```yaml
inventory:
  clients:
    - host: 203.0.113.30
      hostname: norca-win-01
      platform: windows
      connection:
        mode: ssh
        ssh_user: Administrator
        ssh_key_path: ~/.ssh/norca_deployer
      client_settings:
        drivers: [raw_exec]
        enable_raw_exec: true
        native_app_volumes:
          enable_defaults: true     # C:\norca\apps\bin, work, config, data
```

In HCL, Windows paths need doubled backslashes: `data_dir = "D:\\norca\\data"`.

## Scripts

The [scripts of the plan](host-packages-and-scripts.md#scripts) run on Windows nodes in Windows PowerShell 5.1 (`powershell.exe`, which every supported Windows Server has; PowerShell 7 is not needed). `powershell` is the default interpreter of a Windows node, and a `bash` or `sh` script is skipped there. A plan with Linux nodes too names the interpreter of each script (or its `families`), because a script without them would run with `bash` on the Linux nodes and with `powershell` on the Windows nodes; `validate` refuses such a script.

```yaml
scripts:
  - name: windows-firewall
    phase: pre_install
    interpreter: powershell
    path: scripts/windows-firewall.ps1     # examples/nodes/scripts/windows-firewall.ps1
    env:
      CLUSTER_CIDRS: 10.8.0.0/24
      AGENT_PORTS: "4646,4647"
      ALLOCATION_PORTS: 20000-32000
    timeout: 120
```

How a script runs:

- **Files.** The script is uploaded to `C:\ProgramData\norca-deployer\scripts\<name>.ps1`, its environment to `<name>.env.ps1` in the same directory, and the digest of the last successful run is kept in `<name>.sha256`. Before the upload the directory gets an access list of its own: full control for the Administrators and SYSTEM, nothing inherited from `C:\ProgramData` (where every user may read). The uninstall of the node removes `C:\ProgramData\norca-deployer`.
- **Environment.** `<name>.env.ps1` sets one `$env:KEY = '...'` per variable. Each value is a single-quoted PowerShell literal, so quotes, `$`, backticks and line breaks reach the script as they are in the plan; secrets (`!enc:`) are decrypted by the deployer as on Linux. The file is read by the process that starts the script and is deleted after the run; the values never appear on a command line. An empty value leaves the variable unset: Windows has no empty environment variables.
- **Encoding.** The deployer starts both files with a UTF-8 byte order mark (unless the script has one): Windows PowerShell 5.1 reads a file without it in the ANSI code page of the system, which would change every character outside ASCII.
- **Run.** The script runs in a child process, `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File <name>.ps1`. Its output, and the output of the programs it starts, reaches the deployer as Linux output does: printed after a successful run, part of the error of a failed one.
- **Result.** `exit N` ends the script with N; an error that ends the script (`throw`, or any error under `$ErrorActionPreference = 'Stop'`) gives 1; a script that reaches its end gives 0, also when an error that did not end it was written or a native program failed on the way. A result other than 0 fails the node. Start a script with `$ErrorActionPreference = 'Stop'` to stop at the first error, and check `$LASTEXITCODE` after native programs (or end with `exit $LASTEXITCODE`).
- **Timeout.** When `timeout` (default 600 seconds) runs out, the deployer stops the script and every process it started (`taskkill /T /F`: the processes whose parent process is the script or one of its descendants), and the node fails with `the script ran longer than its timeout of <n> s: it and the processes it started were stopped` (exit code 124, as `timeout` gives on Linux). A process whose parent ended before the timeout is no longer part of that tree and keeps running.
- **Execution policy.** `-ExecutionPolicy Bypass` sets the policy of the processes the deployer starts. A policy set by Group Policy takes precedence over it; under `AllSigned` from Group Policy the scripts do not run.
