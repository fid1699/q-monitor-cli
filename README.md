# q-monitor-cli

A cli for basic Quilibrium multi node monitoring.

Don't expect anything fancy. This is not a replacement for proper uptime monitoring (e.g. https://hetrixtools.com/), but at my desk I like to have one terminal monitor up showing application specific stats too, like last frame and peer count.

## Configuration

This implementation uses SSH so it relies on a `.config.json` file with the following format:

```json
{
  "nodes": [
    {
      "ip": "12.13.14.15",
      "username": "user1",
      "password": "password1"
    },
    {
      "ip": "16.17.18.19",
      "username": "user2",
      "password": "password2"
    }
  ]
}
```

For log parsing, running as is assumes your node is running Q as a service named `ceremonyclient`. You can replace the default reader with a tmux reader (which reads logs from a tmux pane of your choice). Adding custom readers is simple enough.

## Running

1. Clone the repo.

```
git clone https://github.com/fid1699/q-monitor-cli.git
cd q-monitor-cli
```

2. Install dependencies

```
go mod tidy
```

3. Run

```
go run monitor.go
```