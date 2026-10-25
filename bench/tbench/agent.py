"""Harbor installed-agent entry point for Wingman."""

from pathlib import Path

from harbor.agents.installed.acp import AcpAgent


class WingmanAgent(AcpAgent):
    """Install a pinned Wingman release and run its ACP stdio server."""

    def __init__(self, *args, **kwargs):
        kwargs.setdefault(
            "registry_entry_path",
            str(Path(__file__).with_name("agent.json")),
        )
        kwargs.setdefault("auth_policy", "disabled")
        kwargs.setdefault("permission_mode", "allow")
        kwargs.setdefault("distribution_preference", "binary")
        super().__init__(*args, **kwargs)
