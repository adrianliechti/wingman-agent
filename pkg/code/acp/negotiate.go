package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	sdk "github.com/coder/acp-go-sdk"
)

// The released Go SDK handles v1. Negotiate before starting either dispatcher
// and hand it the same buffered reader, including any pipelined notifications.
type agentConnection interface {
	Done() <-chan struct{}
	SetLogger(*slog.Logger)
	NewSession(context.Context, sdk.NewSessionRequest) (sdk.NewSessionResponse, error)
	ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error)
	LoadSession(context.Context, sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error)
	ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error)
	CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error)
	DeleteSession(context.Context, sdk.DeleteSessionRequest) (sdk.DeleteSessionResponse, error)
	SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error)
	SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error)
	Prompt(context.Context, sdk.PromptRequest) (sdk.PromptResponse, error)
	Cancel(context.Context, sdk.CancelNotification) error
}

func hostInitialize() sdk.InitializeRequest {
	return sdk.InitializeRequest{
		ProtocolVersion: 1,
		ClientInfo:      &sdk.Implementation{Name: "wingman", Title: new("Wingman"), Version: "0.1.0"},
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:          sdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Elicitation: &sdk.ElicitationCapabilities{Form: &sdk.ElicitationFormCapabilities{}},
			Session:     &sdk.ClientSessionCapabilities{Notices: &sdk.NoticeCapabilities{}},
		},
	}
}

func (a *Agent) connectExternal(ctx context.Context, in io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, a.abortTransport)
	defer stop()
	reader := bufio.NewReader(in)
	legacy := hostInitialize()
	// Older servers can select v1 immediately and read their own capabilities.
	offer := map[string]any{
		"protocolVersion":    2,
		"info":               legacy.ClientInfo,
		"capabilities":       map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
		"clientInfo":         legacy.ClientInfo,
		"clientCapabilities": legacy.ClientCapabilities,
	}
	raw, err := a.initializeFrame(reader, "wingman-init-2", offer)
	var rpc *sdk.RequestError
	if errors.As(err, &rpc) && (rpc.Code == -32602 || rpc.Code == -32022) {
		raw, err = a.initializeFrame(reader, "wingman-init-1", legacy)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	var response struct {
		ProtocolVersion int
		Capabilities    struct {
			Session *struct {
				Delete *struct{}
				Prompt struct{ Image, EmbeddedContext *struct{} }
			}
		}
		Info *sdk.Implementation
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		return err
	}
	switch response.ProtocolVersion {
	case 1:
		var v1 sdk.InitializeResponse
		if err := json.Unmarshal(raw, &v1); err != nil {
			return err
		}
		a.caps = v1.AgentCapabilities
		a.login, a.loginMethod = loginHint(v1.AuthMethods)
		a.title = agentTitle(v1.AgentInfo)
		a.conn = sdk.NewClientSideConnection(a, a.stdin, reader)
	case 2:
		session := response.Capabilities.Session
		if session == nil {
			return errors.New("ACP v2 agent does not advertise sessions")
		}
		if response.Info == nil || response.Info.Name == "" || response.Info.Version == "" {
			return errors.New("invalid ACP v2 implementation information")
		}
		a.caps = sdk.AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: sdk.SessionCapabilities{
				List: &sdk.SessionListCapabilities{}, Resume: &sdk.SessionResumeCapabilities{}, Close: &sdk.SessionCloseCapabilities{},
			},
			PromptCapabilities: sdk.PromptCapabilities{Image: session.Prompt.Image != nil, EmbeddedContext: session.Prompt.EmbeddedContext != nil},
		}
		if session.Delete != nil {
			a.caps.SessionCapabilities.Delete = &sdk.SessionDeleteCapabilities{}
		}
		a.title = agentTitle(response.Info)
		a.conn = newRemoteV2(a, a.stdin, reader)
	default:
		return fmt.Errorf("unsupported ACP protocol version %d", response.ProtocolVersion)
	}
	return nil
}

func (a *Agent) initializeFrame(reader *bufio.Reader, id string, params any) (json.RawMessage, error) {
	if err := json.NewEncoder(a.stdin).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": params}); err != nil {
		return nil, err
	}
	var data []byte
	for {
		line, err := reader.ReadSlice('\n')
		if len(data)+len(line) > 10<<20 {
			return nil, errors.New("ACP initialize response exceeds 10 MiB")
		}
		data = append(data, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		var reply struct {
			JSONRPC, ID string
			Result      json.RawMessage
			Error       *sdk.RequestError
		}
		if err := json.Unmarshal(data, &reply); err != nil {
			return nil, err
		}
		if reply.JSONRPC != "2.0" || reply.ID != id {
			return nil, errors.New("unexpected ACP initialize response")
		}
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}
