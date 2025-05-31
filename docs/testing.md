# Testing

## Run All Tests

```bash
go test ./... -coverprofile=coverage.out
```

## Race Detector

```bash
go test -race ./...
```

## Lint

```bash
golangci-lint run
```

## Vet

```bash
go vet ./...
```

## Coverage

To see a coverage report:

```bash
go tool cover -html=coverage.out
```
