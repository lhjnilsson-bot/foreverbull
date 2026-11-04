package usercopy

// RequestNotRecorded is the sentence for a request stopped because its token
// use couldn't be recorded. The gRPC interceptor doesn't know which fbull
// command sent it.
const RequestNotRecorded = "Couldn't record this request, so foreverbull stopped it. " +
	"Run `fbenv status` to check that the environment is running, then run the same command again."

// SignInExpired is the sentence for a token from `fbull login` that passed its
// 30-day lifetime or its 14-day idle timeout. fbull prints it as sent.
const SignInExpired = "Your sign-in has expired. Run `fbull login` to sign in again."
