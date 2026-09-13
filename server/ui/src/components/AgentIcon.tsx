import { Bot, Pi } from "lucide-react";
import { useColorScheme } from "../hooks/useColorScheme";
import { ModelProviderIcon } from "./ModelProviderIcon";

const agentProviders: Record<string, string> = {
	claude: "anthropic",
	codex: "openai",
	copilot: "github",
	opencode: "opencode",
};

interface Props {
	id: string;
	size?: number;
	className?: string;
}

export function AgentIcon({ id, size = 14, className }: Props) {
	const scheme = useColorScheme();
	const agent = id.trim().toLowerCase();
	if (agent === "wingman") {
		return (
			<img
				src={scheme === "light" ? "/icon_light.svg" : "/icon_dark.svg"}
				width={size}
				height={size}
				className={className}
				alt=""
				aria-hidden="true"
			/>
		);
	}
	const namespace = agentProviders[agent];
	if (!namespace) {
		const Icon = agent === "pi" ? Pi : Bot;
		return <Icon size={size} className={className} aria-hidden="true" />;
	}
	return (
		<ModelProviderIcon
			namespace={namespace}
			size={size}
			className={className}
		/>
	);
}
