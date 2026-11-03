package stream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/lhjnilsson/foreverbull/internal/stream/pb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Message interface {
	GetMsg() *pb.Message
	RawPayload() []byte
	ParsePayload(payload interface{}) error
	Call(ctx context.Context, key Dependency) (interface{}, error)
	MustGet(key Dependency) interface{}

	TaskTopic() string
	TaskAbortTopic() string
	TaskStateTopic() string
}

func NewMessage(module, component, method string, entity any) (Message, error) {
	payload, err := json.Marshal(entity)
	if err != nil {
		return nil, fmt.Errorf("error marshalling payload: %w", err)
	}

	msg := &pb.Message{
		Id:        uuid.New().String(),
		Module:    module,
		Component: component,
		Method:    method,
		Payload:   payload,
	}

	return &message{
		msg: msg,
	}, nil
}

type message struct {
	msg *pb.Message

	dependencyContainer *dependencyContainer
}

func (m *message) GetMsg() *pb.Message {
	return m.msg
}

func (m *message) RawPayload() []byte {
	return m.msg.Payload
}

func (m *message) ParsePayload(v interface{}) error {
	if err := json.Unmarshal(m.msg.Payload, v); err != nil {
		return fmt.Errorf("error unmarshalling payload: %w", err)
	}

	return nil
}

func (m *message) Call(ctx context.Context, key Dependency) (interface{}, error) {
	f, ok := m.dependencyContainer.methods[key]
	if !ok {
		return nil, fmt.Errorf("dependency not found: %s", key)
	}

	return f(ctx, m)
}

func (m *message) MustGet(key Dependency) interface{} {
	v, ok := m.dependencyContainer.singeltons[key]
	if !ok {
		panic(fmt.Sprintf("dependency not found: %s", key))
	}

	return v
}

func (m *message) TaskTopic() string {
	return fmt.Sprintf(TOPIC_TASK, m.msg.GetModule(), m.msg.GetComponent(), m.msg.GetMethod())
}

func (m *message) TaskAbortTopic() string {
	return fmt.Sprintf(TOPIC_TASK_ABORT, m.msg.GetModule(), m.msg.GetComponent(), m.msg.GetMethod(), m.msg.GetId())
}

func (m *message) TaskStateTopic() string {
	return fmt.Sprintf(TOPIC_TASK_STATE, m.msg.GetModule(), m.msg.GetComponent(), m.msg.GetMethod(), m.msg.GetId())
}

func (m *message) taskProgressTopic() string {
	return fmt.Sprintf(TOPIC_TASK_PROGRESS, m.msg.GetModule(), m.msg.GetComponent(), m.msg.GetMethod(), m.msg.GetId())
}

func (m *message) taskUnrecordedTopic() string {
	return fmt.Sprintf(TOPIC_TASK_UNRECORDED, m.msg.GetModule(), m.msg.GetComponent(), m.msg.GetMethod(), m.msg.GetId())
}

func UpdateOrchestration(msg proto.Message, step *pb.Orchestration_Step) error {
	reflectMsg := msg.ProtoReflect()
	msgDesc := reflectMsg.Descriptor()

	fieldDesc := msgDesc.Fields().ByNumber(protoreflect.FieldNumber(step.Num))
	if fieldDesc == nil {
		return fmt.Errorf("field '%d' not found in message", step.Num)
	}

	if fieldDesc.Kind() != protoreflect.MessageKind {
		return fmt.Errorf("field is not an message type")
	}

	stepMsg := reflectMsg.Get(fieldDesc).Message()
	if stepMsg.IsValid() {
		stateField := stepMsg.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(1))
		if stateField == nil {
			return fmt.Errorf("state field not found in message")
		}
		stepMsg.Set(stateField, protoreflect.ValueOfEnum(step.State.Enum().Number()))
		// The message is replaced on every update, so a progress text on a
		// RUNNING step doesn't stay on the step once it completes.
		message := ""
		if step.Error != nil {
			message = *step.Error
		}
		stepMsg.Set(stepMsg.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(2)), protoreflect.ValueOfString(message))
		if len(step.Notices) > 0 {
			noticesField := stepMsg.Descriptor().Fields().ByName("notices")
			if noticesField == nil || !noticesField.IsList() || noticesField.Kind() != protoreflect.StringKind {
				return fmt.Errorf("notices field not found in step message")
			}
			notices := stepMsg.Mutable(noticesField).List()
			notices.Truncate(0)
			for _, line := range step.Notices {
				notices.Append(protoreflect.ValueOfString(line))
			}
		}
	} else {
		return fmt.Errorf("step message not found")
	}
	return nil
}
