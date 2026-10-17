# Docker images

Build images from the repository root so Docker can compile Wingman from the
checkout.

## Worker

The `slim` worker includes Wingman, Git, SSH, curl, and CA certificates on Debian.
It supports Linux amd64 and arm64.

```bash
docker build -f docker/slim/Dockerfile -t wingman-agent:slim .
docker run --rm -i \
  -v "$PWD:/workspace" \
  -e OPENAI_API_KEY wingman-agent:slim acp
```

Set `OPENAI_API_KEY` in the host environment first, or pass your other provider
settings with `-e`. The command above serves ACP over stdio. Without a command
override, the worker listens for ACP TCP connections on port 8080. Use the
container's private network for that listener.

The runtime can select a worker UID/GID with `--user` or Kubernetes security
settings. Make sure that identity can write to the workspace and its home
directory. Language runtimes and project dependencies must be supplied separately.

## Relay

The relay image runs `wingman relay` for remote browser access. See
[remote web access](../docs/remote-control.md#run-the-relay-in-docker) for its
build, HTTPS gateway, and token setup.

With Task installed, `task image:build` builds all image directories locally.
`task image:publish` publishes them to the configured image repository.
