# Debugger examples

Open an individual example directory as a Wingman workspace. Install the
language runtime and build tools listed below, then use **Run | Debug** above
the entry point. The launch dialog offers to install a missing debugger.

| Directory | Required runtime and build tools | Debugger |
|-----------|----------------------------------|----------|
| `go` | Go | Delve |
| `python` | Python | debugpy |
| `java` | JDK and Maven | Microsoft java-debug with JDT LS |
| `typescript` | Node.js and npm | vscode-js-debug |
| `react-vite` | Node.js, npm, and a Chromium browser | vscode-js-debug |
| `rust` | Rust with Cargo and rustup | CodeLLDB |
| `dotnet` | .NET SDK | NetCoreDbg |

In the TypeScript and React examples, run `npm install` first. Review the launch
options for the selected entry point or package script. Wingman can build Rust
and .NET targets as part of launching them. Browser profiles can start a Vite
development server; leave the chosen port free for that launch.

Set a breakpoint by clicking the editor gutter. Use the Debug tab to step,
inspect variables and the call stack, and read output. Closing the Debug tab
stops the session.

Wingman manages language servers and debuggers separately from your project's
runtime. Available debugger packages vary by operating system and architecture;
installation errors appear in the launch dialog with a retry action.

For a C example with compiler flags and navigation across files, see
[the C project](../c/README.md).
