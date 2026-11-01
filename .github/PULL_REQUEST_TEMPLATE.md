## What and why

## How it was checked

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `go test ./...` pass
- [ ] If it can change the disk: it goes through the guards in `actions.go`, and there is a test for it
- [ ] If behaviour a user can see changed: the README says so
