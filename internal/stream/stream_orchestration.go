package stream

import (
	"context"
	"errors"
	"sync"

	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	commonPb "github.com/lhjnilsson/foreverbull/pkg/pb"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

// runOrchestrationStep returns the notices of commands that completed with
// one. A notice is not an error, so it must not cancel the step's other
// commands through the errgroup context.
//
// A command's progress goes to out as a RUNNING step whose Error is the
// text; UpdateOrchestration copies it into the step's message.
func (s *NATSStream) runOrchestrationStep(ctx context.Context, step *pb.Orchestration_Step,
	out chan<- *pb.Orchestration_Step,
) ([]string, error) {
	g, ctx := errgroup.WithContext(ctx)
	progress := func(text string) {
		running := &pb.Orchestration_Step{Num: step.Num, State: commonPb.State_STATE_RUNNING, Error: &text}
		select {
		case out <- running:
		case <-ctx.Done():
		}
	}
	var mu sync.Mutex
	notices := make([]string, 0)
	for _, command := range step.Messages {
		g.Go(func() error {
			msg := &message{msg: command}
			err := s.Publish(ctx, msg, WaitForCompletion(progress))
			var notice *Notice
			if errors.As(err, &notice) {
				mu.Lock()
				notices = append(notices, notice.Lines...)
				mu.Unlock()
				return nil
			}
			return err
		})
	}
	return notices, g.Wait()
}

// StepNotRecorded reports whether step failed because the stream was full
// (ErrStateNotRecorded). A step carries its error as text, so the check is
// on the sentinel's text, the same way commandError reads a task state.
func StepNotRecorded(step *pb.Orchestration_Step) bool {
	return step.GetState() == commonPb.State_STATE_FAILED && step.GetError() == ErrStateNotRecorded.Error()
}

func (s *NATSStream) RunOrchestration(ctx context.Context, o *pb.Orchestration) <-chan *pb.Orchestration_Step {
	out := make(chan *pb.Orchestration_Step)

	go func() {
		defer close(out)
		for _, step := range o.Steps {
			runningStep := pb.Orchestration_Step{Num: step.Num, State: commonPb.State_STATE_RUNNING}

			out <- &runningStep
			notices, err := s.runOrchestrationStep(ctx, step, out)
			if err != nil {
				log.Warn().Err(err).Msg("error running orchestration step")
				errStr := err.Error()
				failedStep := pb.Orchestration_Step{Num: step.Num, State: commonPb.State_STATE_FAILED, Error: &errStr}
				out <- &failedStep
				return
			}
			// Error stays nil: a completed step never carries one.
			// UpdateOrchestration copies Notices into the step's notices.
			completedStep := pb.Orchestration_Step{Num: step.Num, State: commonPb.State_STATE_COMPLETED, Notices: notices}
			out <- &completedStep
		}
	}()
	return out
}
