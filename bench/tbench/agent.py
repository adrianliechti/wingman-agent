"""Harbor installed-agent entry point for Wingman."""

from pathlib import Path

from harbor.agents.installed.acp import AcpAgent
from harbor.models.agent.context import AgentContext


class WingmanAgent(AcpAgent):
    """Install a pinned Wingman release and run its ACP stdio server."""

    _ACP_PYTHON_SDK_VERSION = "0.10.1"

    def __init__(self, *args, **kwargs):
        kwargs.setdefault(
            "registry_entry_path",
            str(Path(__file__).with_name("agent.json")),
        )
        kwargs.setdefault("auth_policy", "disabled")
        kwargs.setdefault("permission_mode", "allow")
        kwargs.setdefault("distribution_preference", "binary")
        super().__init__(*args, **kwargs)

    def _build_dependencies_command(self, kind):
        """Keep Harbor 0.18's runner on the ACP API it was written for."""
        command = super()._build_dependencies_command(kind)
        dependency = "pip install agent-client-protocol"
        if dependency not in command:
            raise RuntimeError("Harbor's ACP dependency installer changed")
        return command.replace(
            dependency,
            f"{dependency}=={self._ACP_PYTHON_SDK_VERSION}",
        )

    def populate_context_post_run(self, context: AgentContext) -> None:
        """Preserve cached-token usage omitted by Harbor 0.18's ACP adapter."""
        super().populate_context_post_run(context)
        summary = self._load_summary() or {}
        usage = (summary.get("prompt_response") or {}).get("usage") or {}
        cached_tokens = usage.get("cachedReadTokens")
        if isinstance(cached_tokens, int | float):
            context.n_cache_tokens = int(cached_tokens)
