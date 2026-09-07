package session

import (
	"context"
	"strings"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

type Transcript interface {
	ReadFrom(offset int) (text string, next int)
}

type StreamExecOptions struct {
	Transcript     Transcript
	Reader         vmruntime.TranscriptReader
	Start          int
	ID             string
	OnEvent        func(client.ExecEvent) error
	OnCallbackFail func()
	OnContextDone  func()
	OnObserve      func(StreamExecObservation)
	Wait           func(context.Context) error
}

type StreamExecObservation struct {
	Kind     string
	Line     string
	Duration time.Duration
	Matched  bool
	Done     bool
	Stats    StreamExecStats
}

type StreamExecStats struct {
	Loops   int
	Reads   int
	Lines   int
	Matched int
	Ignored int
	Waits   int
}

func StreamExecEvents(ctx context.Context, opts StreamExecOptions) error {
	reader := opts.Reader
	if reader == nil {
		if retained, ok := opts.Transcript.(interface {
			RetainReader(int) vmruntime.TranscriptReader
		}); ok {
			reader = retained.RetainReader(opts.Start)
			defer reader.Close()
		}
	}
	offset := opts.Start
	var pending string
	var stats StreamExecStats
	totalStart := time.Now()
	observe := func(obs StreamExecObservation) {
		if opts.OnObserve != nil {
			opts.OnObserve(obs)
		}
	}
	for {
		stats.Loops++
		observe(StreamExecObservation{Kind: "loop", Stats: stats})
		text := ""
		next := offset
		readStart := time.Now()
		if opts.Transcript != nil {
			text, next = opts.Transcript.ReadFrom(offset)
		}
		observe(StreamExecObservation{Kind: "transcript_read", Duration: time.Since(readStart), Stats: stats})
		if len(text) > 0 {
			stats.Reads++
			appendStart := time.Now()
			pending += text
			offset = next
			if reader != nil {
				reader.Advance(next)
			}
			observe(StreamExecObservation{Kind: "append_pending", Duration: time.Since(appendStart), Stats: stats})
			for {
				lineStart := time.Now()
				lineEnd := strings.IndexByte(pending, '\n')
				if lineEnd < 0 {
					break
				}
				line := strings.TrimSpace(pending[:lineEnd])
				pending = pending[lineEnd+1:]
				stats.Lines++
				observe(StreamExecObservation{Kind: "line", Line: line, Duration: time.Since(lineStart), Stats: stats})
				parseStart := time.Now()
				event, done, ok, err := vmruntime.ParseManagedExecEventLine(line, opts.ID)
				parseDuration := time.Since(parseStart)
				if err != nil {
					return err
				}
				if !ok {
					stats.Ignored++
					observe(StreamExecObservation{Kind: "parse", Line: line, Duration: parseDuration, Matched: false, Done: done, Stats: stats})
					continue
				}
				stats.Matched++
				observe(StreamExecObservation{Kind: "parse", Line: line, Duration: parseDuration, Matched: true, Done: done, Stats: stats})
				if opts.OnEvent != nil {
					callbackStart := time.Now()
					if err := opts.OnEvent(event); err != nil {
						observe(StreamExecObservation{Kind: "callback", Duration: time.Since(callbackStart), Matched: true, Done: done, Stats: stats})
						if opts.OnCallbackFail != nil {
							opts.OnCallbackFail()
						}
						return err
					}
					observe(StreamExecObservation{Kind: "callback", Duration: time.Since(callbackStart), Matched: true, Done: done, Stats: stats})
				}
				if done {
					observe(StreamExecObservation{Kind: "done", Duration: time.Since(totalStart), Matched: true, Done: true, Stats: stats})
					return nil
				}
			}
			continue
		}
		if ctx.Err() != nil {
			if opts.OnContextDone != nil {
				opts.OnContextDone()
			}
			return ctx.Err()
		}
		if opts.Wait != nil {
			stats.Waits++
			waitStart := time.Now()
			if err := opts.Wait(ctx); err != nil {
				observe(StreamExecObservation{Kind: "wait", Duration: time.Since(waitStart), Stats: stats})
				return err
			}
			observe(StreamExecObservation{Kind: "wait", Duration: time.Since(waitStart), Stats: stats})
		}
	}
}
