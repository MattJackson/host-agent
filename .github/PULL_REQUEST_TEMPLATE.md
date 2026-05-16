<!--
Thanks for the PR! A few things to check off before merge:
-->

## Summary

<!-- One or two sentences. What does this change, and why? -->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] New chassis profile (`profiles/<model>.env`)
- [ ] Docs / README / CONTRIBUTING
- [ ] Refactor / internal cleanup
- [ ] CI / build / release plumbing

## Checklist

- [ ] `go test ./...` passes locally.
- [ ] `gofmt -s -l .` is clean (no output).
- [ ] `go vet ./...` is clean.
- [ ] If this touches the controller math: a unit test demonstrates the
      before/after, and existing tests still pass.
- [ ] If this adds a chassis profile: PR description includes chassis
      model, BMC firmware version, observed fan-floor behavior, and a
      sample `metrics.prom` from running the agent for a few hours.
- [ ] No new external Go dependencies. (If yes, justify in the
      description.)
- [ ] README / CHANGELOG updated if user-facing behavior changed.
- [ ] Commit messages are clean (no `Co-Authored-By: AI`-style
      trailers).
