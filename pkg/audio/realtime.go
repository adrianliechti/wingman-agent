package audio

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/realtime"
	"github.com/openai/openai-go/v3/responses"
)

const RealtimeSampleRate = 24000

// Realtime uses the configured OpenAI-compatible endpoint and credentials.
type Realtime struct {
	Client openai.Client
	Model  string
}

// IsRealtimeModel recognizes the Realtime API, which has a different wire
// protocol from transcription and GPT-Live sessions.
func IsRealtimeModel(id string) bool {
	id = strings.ToLower(id)
	return strings.Contains(id, "realtime") && !strings.Contains(id, "transcrib") && !strings.Contains(id, "preview")
}

func (r *Realtime) Connect(ctx context.Context, instructions string, tools realtime.RealtimeToolsConfigParam) (*realtime.RealtimeConnection, error) {
	opening, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := r.Client.Realtime.Connect(opening, realtime.RealtimeConnectionOptions{}, option.WithQuery("model", r.Model))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(opening, conn.Abort)
	defer stop()
	format := realtime.RealtimeAudioFormatsUnionParam{OfAudioPCM: &realtime.RealtimeAudioFormatsAudioPCMParam{Type: "audio/pcm", Rate: RealtimeSampleRate}}
	// The connection URL selects the model. Leave Model unset in session.update.
	config := realtime.RealtimeSessionCreateRequestParam{
		Instructions:     openai.String(instructions),
		OutputModalities: []string{"audio"},
		Audio: realtime.RealtimeAudioConfigParam{
			Input: realtime.RealtimeAudioConfigInputParam{
				Format: format,
				TurnDetection: realtime.RealtimeAudioInputTurnDetectionUnionParam{OfServerVad: &realtime.RealtimeAudioInputTurnDetectionServerVadParam{
					// Server VAD finds the turns and cuts off a reply the user talks
					// over. The caller creates each reply, so it knows what it answers.
					CreateResponse: openai.Bool(false), InterruptResponse: openai.Bool(true),
				}},
			},
			Output: realtime.RealtimeAudioConfigOutputParam{Format: format, Voice: realtime.RealtimeAudioConfigOutputVoiceUnionParam{OfString: openai.String("marin")}},
		},
		Tools:      tools,
		ToolChoice: realtime.RealtimeToolChoiceConfigUnionParam{OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto)},
	}
	err = conn.Send(opening, realtime.RealtimeClientEventParamOfSessionUpdate(config))
	for err == nil {
		var event realtime.RealtimeServerEventUnion
		event, err = conn.Recv(opening)
		if err != nil {
			break
		}
		switch event := event.AsAny().(type) {
		case realtime.SessionUpdatedEvent:
			if err := opening.Err(); err != nil {
				conn.Abort()
				return nil, err
			}
			return conn, nil
		case realtime.RealtimeErrorEvent:
			err = fmt.Errorf("voice: %s", event.Error.Message)
		}
	}
	conn.Abort()
	return nil, err
}
