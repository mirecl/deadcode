
Version deadcode: **v0.35.0**
Integration for **golangci-lint**:

```yaml
linters-settings:
  custom:
    deadcode:
      type: "module"
      description: finds unreachable funcs.
      settings:
        test: false
        filter: (calc|res)
```
