import {
	renderTokens,
	tokenize,
	type HighlightRenderNode,
} from "@tanstack/highlight";
import { parseCodeFenceMeta } from "@tanstack/highlight/markdown";
import { streamingMarkdownExtension } from "@tanstack/markdown/extensions/streaming";
import {
	Markdown,
	type MarkdownComponentProps,
	type MarkdownComponents,
} from "@tanstack/markdown/react";
import { Check, Code2, Copy, Image } from "lucide-react";
import {
	Children,
	createContext,
	isValidElement,
	memo,
	useContext,
	useEffect,
	useRef,
	useState,
	type MouseEvent,
	type ReactElement,
	type ReactNode,
} from "react";
import {
	autolinkExtension,
	findUrls,
	type UrlMatch,
} from "../utils/markdownAutolink";
import { MermaidPreview } from "./MermaidPreview";

const MARKDOWN_EXTENSIONS = [streamingMarkdownExtension(), autolinkExtension()];
const MarkdownStreamingContext = createContext(false);

// Code links only open on Ctrl/Cmd+click, like in an editor, so a plain click
// still selects text.
function openOnModifierClick(event: MouseEvent<HTMLAnchorElement>) {
	if (!event.metaKey && !event.ctrlKey) event.preventDefault();
}

// Wraps the URL parts of text[start, end) in Ctrl/Cmd+click links. `urls` are
// found on the whole source, so a URL spanning several tokens still links.
function withCodeLinks(
	text: string,
	urls: UrlMatch[],
	start = 0,
	end = text.length,
	firstUrl = 0,
): ReactNode[] {
	const parts: ReactNode[] = [];
	let cursor = start;
	for (let i = firstUrl; i < urls.length && urls[i].start < end; i++) {
		const url = urls[i];
		const from = Math.max(url.start, cursor);
		const to = Math.min(url.end, end);
		if (from >= to) continue;
		if (from > cursor) parts.push(text.slice(cursor, from));
		parts.push(
			<a
				key={from}
				href={url.href}
				target="_blank"
				rel="noopener noreferrer"
				title="Ctrl/Cmd+click to open"
				className="md-code-link"
				onClick={openOnModifierClick}
			>
				{text.slice(from, to)}
			</a>,
		);
		cursor = to;
	}
	if (cursor < end) parts.push(text.slice(cursor, end));
	return parts;
}

function highlightedCode(
	code: string,
	language: string,
	options: ReturnType<typeof parseCodeFenceMeta>,
): ReactNode[] {
	// Render tokens as React nodes to keep code links and the original copy text.
	// The view helpers trim source text; tokenize/renderTokens preserve it.
	const { tokens } = tokenize(code, { lang: language });
	const urls = findUrls(code);
	let offset = 0;
	let urlIndex = 0;

	function renderNode(node: HighlightRenderNode, key: number): ReactNode {
		if (node.type === "text") {
			const start = offset;
			offset += node.value.length;
			while (urlIndex < urls.length && urls[urlIndex].end <= start) urlIndex++;
			return withCodeLinks(code, urls, start, offset, urlIndex);
		}
		return (
			<span
				key={key}
				className={node.classNames.join(" ")}
				data-line={node.data?.line}
			>
				{node.children.map(renderNode)}
			</span>
		);
	}

	return renderTokens(tokens, options).map(renderNode);
}

type CodePreProps = MarkdownComponentProps<"pre"> & {
	"data-code-title"?: string;
	"data-filename"?: string;
	"data-lang"?: string;
	"data-meta"?: string;
};

function nodeText(node: ReactNode): string {
	return Children.toArray(node)
		.map((child) =>
			typeof child === "string" || typeof child === "number"
				? String(child)
				: isValidElement<{ children?: ReactNode }>(child)
					? nodeText(child.props.children)
					: "",
		)
		.join("");
}

function MarkdownCodeBlock({ children, ...props }: CodePreProps) {
	const codeElement = Children.toArray(children).find(
		(child): child is ReactElement<{ children?: ReactNode }> =>
			isValidElement<{ children?: ReactNode }>(child),
	);
	const code = codeElement ? nodeText(codeElement.props.children) : "";
	const language = props["data-lang"] ?? "plaintext";
	const fence = parseCodeFenceMeta(props["data-meta"]);
	const label =
		props["data-filename"] ??
		props["data-code-title"] ??
		fence.title ??
		language;
	const [copied, setCopied] = useState(false);
	const [showMermaidSource, setShowMermaidSource] = useState(false);
	const copyTimer = useRef<number | undefined>(undefined);
	const mermaid = language.trim().toLowerCase() === "mermaid";
	const streaming = useContext(MarkdownStreamingContext);

	useEffect(
		() => () => {
			if (copyTimer.current !== undefined)
				window.clearTimeout(copyTimer.current);
		},
		[],
	);

	function copyCode() {
		void navigator.clipboard.writeText(code).then(() => {
			setCopied(true);
			if (copyTimer.current !== undefined)
				window.clearTimeout(copyTimer.current);
			copyTimer.current = window.setTimeout(() => setCopied(false), 1500);
		});
	}

	if (mermaid) {
		return (
			<div data-markdown-code data-language={language}>
				<div data-markdown-code-header>
					<span>
						{label === "mermaid" ? "diagram" : label}
						{streaming ? " · generating…" : ""}
					</span>
					<span className="flex items-center gap-0.5">
						<button
							type="button"
							disabled={streaming}
							onClick={() => setShowMermaidSource((show) => !show)}
							aria-label={
								streaming
									? "Diagram available when generation finishes"
									: showMermaidSource
										? "Show diagram"
										: "Show Mermaid source"
							}
							title={
								streaming
									? "Diagram available when complete"
									: showMermaidSource
										? "Show diagram"
										: "Show source"
							}
						>
							{showMermaidSource ? (
								<Image aria-hidden="true" />
							) : (
								<Code2 aria-hidden="true" />
							)}
						</button>
						<button
							type="button"
							onClick={copyCode}
							aria-label={
								copied ? "Diagram source copied" : "Copy diagram source"
							}
							title={copied ? "Copied" : "Copy source"}
						>
							{copied ? (
								<Check aria-hidden="true" />
							) : (
								<Copy aria-hidden="true" />
							)}
						</button>
					</span>
				</div>
				{streaming || showMermaidSource ? (
					<pre {...props}>
						<code className="language-mermaid">{code}</code>
					</pre>
				) : (
					<div className="h-96 min-h-72 overflow-hidden rounded border border-border-subtle bg-bg">
						<MermaidPreview text={code} path={label} />
					</div>
				)}
			</div>
		);
	}

	return (
		<div data-markdown-code data-language={language}>
			<div data-markdown-code-header>
				<span>{label}</span>
				<button
					type="button"
					onClick={copyCode}
					aria-label={copied ? "Code copied" : "Copy code"}
					title={copied ? "Copied" : "Copy code"}
				>
					{copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
				</button>
			</div>
			<pre
				{...props}
				className={`${props.className ?? ""}${fence.lineNumbers ? " th-code--line-numbers" : ""}`}
			>
				<code className={`language-${language}`}>
					{highlightedCode(code, language, fence)}
				</code>
			</pre>
		</div>
	);
}

const components = {
	a({ href, children, ...props }: MarkdownComponentProps<"a">) {
		const internal = href?.startsWith("#");
		return (
			<a
				{...props}
				href={href}
				target={internal ? undefined : "_blank"}
				rel={internal ? undefined : "noopener noreferrer"}
				className="text-accent hover:text-accent-hover underline decoration-border-strong underline-offset-2"
			>
				{children}
			</a>
		);
	},
	h1({ children }: MarkdownComponentProps<"h1">) {
		return (
			<h1 className="mt-4 mb-2 font-semibold text-[15px] text-fg">
				{children}
			</h1>
		);
	},
	h2({ children, className, ...props }: MarkdownComponentProps<"h2">) {
		return (
			<h2
				{...props}
				className={
					className ?? "mt-3.5 mb-1.5 font-semibold text-[14px] text-fg"
				}
			>
				{children}
			</h2>
		);
	},
	h3({ children }: MarkdownComponentProps<"h3">) {
		return (
			<h3 className="mt-3 mb-1.5 font-semibold text-[13px] text-fg">
				{children}
			</h3>
		);
	},
	p({ children }: MarkdownComponentProps<"p">) {
		return (
			<p className="mb-2 last:mb-0 empty:hidden text-fg-muted">{children}</p>
		);
	},
	ul({ children }: MarkdownComponentProps<"ul">) {
		return <ul className="md-list md-ul text-fg-muted">{children}</ul>;
	},
	ol({ children, ...props }: MarkdownComponentProps<"ol">) {
		return (
			<ol {...props} className="md-list md-ol text-fg-muted">
				{children}
			</ol>
		);
	},
	li({ children, ...props }: MarkdownComponentProps<"li">) {
		return (
			<li {...props} className="mb-0.5">
				{children}
			</li>
		);
	},
	blockquote({ children }: MarkdownComponentProps<"blockquote">) {
		return (
			<blockquote className="border-l-2 border-border pl-2.5 my-1.5 text-fg-dim">
				{children}
			</blockquote>
		);
	},
	strong({ children }: MarkdownComponentProps<"strong">) {
		return <strong className="font-semibold text-fg">{children}</strong>;
	},
	em({ children }: MarkdownComponentProps<"em">) {
		return <em className="text-fg-muted">{children}</em>;
	},
	del({ children }: MarkdownComponentProps<"del">) {
		return <del className="text-fg-dim">{children}</del>;
	},
	table({ children, ...props }: MarkdownComponentProps<"table">) {
		return (
			<div data-markdown-table>
				<table {...props}>{children}</table>
			</div>
		);
	},
	code({ children, ...props }: MarkdownComponentProps<"code">) {
		return (
			<code {...props}>
				{typeof children === "string"
					? withCodeLinks(children, findUrls(children))
					: children}
			</code>
		);
	},
	pre: MarkdownCodeBlock,
} satisfies MarkdownComponents;

export const MarkdownContent = memo(function MarkdownContent({
	text,
	streaming = false,
}: {
	text: string;
	streaming?: boolean;
}) {
	if (!text?.trim()) return null;

	return (
		<MarkdownStreamingContext value={streaming}>
			<div data-markdown-content data-streaming={streaming || undefined}>
				<Markdown
					components={components}
					extensions={MARKDOWN_EXTENSIONS}
					frontmatter={false}
					headingIds={false}
				>
					{text}
				</Markdown>
			</div>
		</MarkdownStreamingContext>
	);
});
