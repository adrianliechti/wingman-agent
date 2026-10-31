package acp

import (
	"context"
	"reflect"

	"github.com/coder/acp-go-sdk"
)

// Notify sends one session update, wrapping the notification envelope every
// bridge would otherwise spell out at each call site.
func Notify(ctx context.Context, conn ClientConnection, sid acp.SessionId, update acp.SessionUpdate) error {
	ctx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()
	_, err := Call(ctx, func() (struct{}, error) {
		return struct{}{}, conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: update})
	})
	return err
}

// Configuration updates replace the complete list, including options removed
// by a model change. Publish before returning a successful setter response.
func NotifyConfig(ctx context.Context, conn ClientConnection, sid acp.SessionId, before, after []acp.SessionConfigOption) error {
	if conn == nil || reflect.DeepEqual(before, after) {
		return nil
	}
	return Notify(ctx, conn, sid, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		SessionUpdate: "config_option_update", ConfigOptions: after,
	}})
}
