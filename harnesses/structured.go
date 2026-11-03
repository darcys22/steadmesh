package harnesses

import (
	"context"
	"errors"
	"fmt"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

const (
	StructuredOutputPlatform = "platform_validated"
	StructuredOutputNative   = "native"
)

var (
	ErrStructuredOutput            = errors.New("harness: invalid structured output")
	ErrStructuredOutputUnsupported = errors.New("harness: structured output unsupported")
)

// DeliverValidated is the common structured-output boundary. Ordinary text
// deliveries retain their previous behaviour. A schema mismatch is an output
// failure, never a reason to replay a potentially side-effectful turn here.
func DeliverValidated(ctx context.Context, a Adapter, d Delivery) (TurnResult, error) {
	if len(d.OutputSchema) == 0 {
		return a.Deliver(ctx, d)
	}
	if err := runtimeapi.ValidateOutputSchema(d.OutputSchema); err != nil {
		return TurnResult{}, err
	}
	mode := a.DescribeCapabilities().StructuredOutputMode
	if mode != StructuredOutputPlatform && mode != StructuredOutputNative {
		return TurnResult{}, ErrStructuredOutputUnsupported
	}
	turn, err := a.Deliver(ctx, d)
	if err != nil || turn.Status != TurnCompleted {
		return turn, err
	}
	raw := turn.StructuredOutput
	if len(raw) == 0 {
		raw = []byte(turn.Output)
	}
	valid, err := runtimeapi.ValidateStructuredOutput(d.OutputSchema, raw)
	if err != nil {
		turn.Status, turn.Error, turn.StructuredOutput = TurnFailed, runtimeapi.StructuredOutputInvalid, nil
		return turn, fmt.Errorf("%w: %s", ErrStructuredOutput, err)
	}
	turn.StructuredOutput = valid
	turn.Output = string(valid)
	return turn, nil
}
