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
| File transfer | SFTP paths in the form `/C:/...` |
| Binary update | The running `norca.exe` cannot be overwritten: the new binary is staged as `norca.exe.new` and swapped in while the service is stopped |
| Service | `norca` service created with `New-Service`, quoted `ImagePath` (paths with spaces), automatic start, restart on failure |
| Firewall | Not configured: open the ports in Windows Firewall yourself (see [Networking and tuning](networking-and-tuning.md#firewall) for the ports) |
| Tuning | `tuning.windows_long_paths: true` sets `LongPathsEnabled = 1` and the execution policy `RemoteSigned` |
| Paths | Separate defaults under `C:\Program Files\norca` and `C:\ProgramData\norca` (see [Networking and tuning](networking-and-tuning.md)) |
| Host packages, scripts | Skipped with a warning |
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
