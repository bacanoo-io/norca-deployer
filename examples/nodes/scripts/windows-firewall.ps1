# Example script of a Windows node: opens the ports of the Norca client in
# Windows Firewall, which the deployer does not configure, for the cluster
# network only. It runs with Windows PowerShell 5.1 (interpreter
# powershell). Safe to run again.
$ErrorActionPreference = 'Stop'

$sources = $env:CLUSTER_CIDRS -split ','
$rules = [ordered]@{
  'norca-agent'       = $env:AGENT_PORTS        # HTTP and RPC of the agent
  'norca-allocations' = $env:ALLOCATION_PORTS   # the ports of the allocations
}
foreach ($name in $rules.Keys) {
  if (Get-NetFirewallRule -Name $name -ErrorAction SilentlyContinue) {
    Remove-NetFirewallRule -Name $name
  }
  New-NetFirewallRule -Name $name -DisplayName "Norca ($name)" -Direction Inbound -Action Allow `
    -Protocol TCP -LocalPort ($rules[$name] -split ',') -RemoteAddress $sources | Out-Null
  Write-Output "firewall rule ${name}: TCP $($rules[$name]) from $($sources -join ', ')"
}
