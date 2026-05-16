# Security policy

## Supported versions

Only the **latest tagged release** receives security fixes. There is no
LTS branch. Update to the most recent tag before reporting an issue.

## Reporting a vulnerability

`host-agent` runs as a `--privileged` container with `/dev`, `/sys`, and
the Docker socket mounted, so security issues here are usually
host-level (container escape, privilege escalation, arbitrary IPMI raw
commands, etc.). Please treat them accordingly.

**Do not open a public GitHub issue for a vulnerability.** Use GitHub
Security Advisories instead:

→ <https://github.com/OWNER/host-agent/security/advisories/new>

This routes the report privately to the maintainers, gives us a place
to coordinate a fix, and gets you a CVE if one's warranted.

Include in your report:

- A short description of the issue
- Steps to reproduce (commands, env, hardware if relevant)
- The version (image tag / git SHA) you tested against
- Your assessment of impact

We aim to acknowledge within 7 days and ship a fix or disclosure
timeline within 30 days. We don't currently run a bug bounty.

## Scope

In-scope:

- The container itself (Dockerfile, s6 service tree)
- The Go binary (`cmd/fan-controller/`, `internal/`)
- The shell wrappers (`s6/*/run`, `install/install.sh`,
  `infra/host-requirements.sh`)
- The Unraid CA template (`install/host-agent.xml`)

Out of scope:

- Upstream vulnerabilities in the bundled binaries (`node_exporter`,
  `cadvisor`, `ipmi_exporter`, `smartctl_exporter`,
  `nvidia_gpu_exporter`, `vmagent`). Report those to the respective
  upstreams; we'll pick up the fix on our next rebuild.
- Misconfigurations on the operator's host (exposed Prometheus
  endpoint, leaked bearer token in `.env` files committed to public
  repos, etc.). Document these as deployment guidance, not
  vulnerabilities.

## What we won't fix

`--privileged` is a hard requirement for the container's job (IPMI fan
control + drive SMART + textfile collector all need it). "Run with
less privilege" reports get a polite no.
