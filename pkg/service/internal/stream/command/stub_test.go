package command_test

import (
	"context"
	"testing"

	"github.com/lhjnilsson/foreverbull/internal/stream"
	"github.com/lhjnilsson/foreverbull/pkg/service/internal/stream/command"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A command with no implementation answers Unimplemented, never success,
// and its text is the plain sentence with no gRPC prefix.
func TestUnimplementedCommands(t *testing.T) {
	cases := []struct {
		name string
		run  func(context.Context, stream.Message) error
		want string
	}{
		{"service status", command.UpdateServiceStatus, "This version of foreverbull can't report a strategy service's status. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."},
		{"instance interview", command.InstanceInterview, "This version of foreverbull can't ask a strategy worker for its setup when it starts. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."},
		{"instance sanity check", command.InstanceSanityCheck, "This version of foreverbull can't check that a strategy worker is ready when it starts. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."},
		{"instance stop", command.InstanceStop, "This version of foreverbull can't stop a strategy worker. Please report it at https://github.com/lhjnilsson/foreverbull/issues with this message."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := new(stream.MockMessage)
			msg.On("ParsePayload", mock.Anything).Return(nil)
			err := c.run(context.Background(), msg)
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.Unimplemented, st.Code())
			require.Equal(t, c.want, st.Message())
			// The ERROR state carries the error's text: the sentence only.
			require.EqualError(t, err, c.want)
		})
	}
}
