package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	duckruntime "github.com/hackerduck/duckway/internal/ducklion/runtime"
)

type SupervisorClient struct {
	conn       *net.UnixConn
	codec      *bridge.Codec
	identity   protocol.SupervisorRegistered
	instanceID model.InstanceID
	mu         sync.Mutex
	nextOffset uint64
	nextID     uint64
	socketPath string
	privateKey ed25519.PrivateKey
}

func RegisterSupervisor(socketPath string, sessionID model.SessionID, generation uint64, privateKey ed25519.PrivateKey) (*SupervisorClient, error) {
	if _, err := model.ParseSessionID(string(sessionID)); err != nil {
		return nil, err
	}
	if generation == 0 || len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("runtime generation and recovery private key are required")
	}
	connection, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	conn := connection.(*net.UnixConn)
	fail := func(err error) (*SupervisorClient, error) { _ = conn.Close(); return nil, err }
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	handshake := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: protocol.RoleSupervisor,
		Principal: string(sessionID), Capabilities: []string{"supervisor_recovery", "output_publish"}}
	if err := codec.Write(handshake); err != nil {
		return fail(err)
	}
	var handshakeResponse protocol.HandshakeResponse
	if err := codec.Read(&handshakeResponse); err != nil {
		return fail(err)
	}
	if err := handshakeResponse.Validate(); err != nil {
		return fail(err)
	}
	if handshakeResponse.Error != nil {
		return fail(fmt.Errorf("supervisor handshake rejected: %s", handshakeResponse.Error.Message))
	}
	negotiated := handshakeResponse.Handshake
	if negotiated.Major != protocol.Major || negotiated.Minor < 0 || negotiated.Minor > protocol.Minor || negotiated.Role != protocol.RoleSupervisor ||
		negotiated.Principal != string(sessionID) || !hasCapability(negotiated.Capabilities, "supervisor_recovery") || !hasCapability(negotiated.Capabilities, "output_publish") {
		return fail(fmt.Errorf("supervisor returned an invalid handshake"))
	}
	if err := codec.Write(protocol.Request{ID: "register-begin", Type: "supervisor.register_begin", SessionID: string(sessionID), RuntimeGeneration: &generation}); err != nil {
		return fail(err)
	}
	var beginResponse protocol.Response
	if err := codec.Read(&beginResponse); err != nil {
		return fail(err)
	}
	if err := validateResponse(beginResponse, "register-begin"); err != nil {
		return fail(err)
	}
	if beginResponse.Error != nil {
		return fail(fmt.Errorf("supervisor registration rejected: %s", beginResponse.Error.Message))
	}
	var challenge protocol.SupervisorChallenge
	if err := json.Unmarshal(beginResponse.Result, &challenge); err != nil {
		return fail(err)
	}
	if challenge.ChallengeID == "" || len(challenge.Nonce) != model.RecoveryNonceBytes {
		return fail(fmt.Errorf("supervisor returned an invalid registration challenge"))
	}
	instanceID, err := model.ParseInstanceID(challenge.InstanceID)
	if err != nil {
		return fail(err)
	}
	proof := model.RecoveryProof(privateKey, instanceID, sessionID, generation, challenge.Nonce, uint16(negotiated.Major), uint16(negotiated.Minor))
	completeBody, _ := json.Marshal(protocol.SupervisorRegisterComplete{ChallengeID: challenge.ChallengeID, Proof: proof})
	if err := codec.Write(protocol.Request{ID: "register-complete", Type: "supervisor.register_complete", InstanceID: string(instanceID), SessionID: string(sessionID), RuntimeGeneration: &generation, Body: completeBody}); err != nil {
		return fail(err)
	}
	var completeResponse protocol.Response
	if err := codec.Read(&completeResponse); err != nil {
		return fail(err)
	}
	if err := validateResponse(completeResponse, "register-complete"); err != nil {
		return fail(err)
	}
	if completeResponse.Error != nil {
		return fail(fmt.Errorf("supervisor registration rejected: %s", completeResponse.Error.Message))
	}
	var identity protocol.SupervisorRegistered
	if err := json.Unmarshal(completeResponse.Result, &identity); err != nil {
		return fail(err)
	}
	if identity.SessionID != string(sessionID) || identity.RuntimeGeneration != generation || identity.LeaseID == "" {
		return fail(fmt.Errorf("supervisor returned an invalid runtime identity"))
	}
	_ = conn.SetDeadline(time.Time{})
	return &SupervisorClient{conn: conn, codec: codec, identity: identity, instanceID: instanceID, socketPath: socketPath,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...)}, nil
}

type RuntimeController interface {
	SubmitInput(context.Context, duckruntime.InputFrame) error
	Resize(rows, cols uint16, epoch, generation uint64) error
}

func (c *SupervisorClient) ServeControl(ctx context.Context, controller RuntimeController) error {
	connection, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return err
	}
	conn := connection.(*net.UnixConn)
	defer conn.Close()
	codec := bridge.NewCodec(conn, conn, bridge.DefaultMaxFrame)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	handshake := protocol.Handshake{Major: protocol.Major, Minor: protocol.Minor, Role: protocol.RoleSupervisorControl,
		Principal: c.identity.SessionID, Capabilities: []string{"runtime_control"}}
	if err := codec.Write(handshake); err != nil {
		return err
	}
	var handshakeResponse protocol.HandshakeResponse
	if err := codec.Read(&handshakeResponse); err != nil {
		return err
	}
	if err := handshakeResponse.Validate(); err != nil || handshakeResponse.Error != nil {
		return fmt.Errorf("runtime control handshake rejected")
	}
	negotiated := handshakeResponse.Handshake
	if negotiated.Major != protocol.Major || negotiated.Minor < 0 || negotiated.Minor > protocol.Minor || negotiated.Role != protocol.RoleSupervisorControl ||
		negotiated.Principal != c.identity.SessionID || !hasCapability(negotiated.Capabilities, "runtime_control") {
		return fmt.Errorf("runtime control returned an invalid handshake")
	}
	generation := c.identity.RuntimeGeneration
	if err := codec.Write(protocol.Request{ID: "control-begin", Type: "supervisor.control_begin", SessionID: c.identity.SessionID, RuntimeGeneration: &generation}); err != nil {
		return err
	}
	var beginResponse protocol.Response
	if err := codec.Read(&beginResponse); err != nil {
		return err
	}
	if err := validateResponse(beginResponse, "control-begin"); err != nil || beginResponse.Error != nil {
		return fmt.Errorf("runtime control authentication rejected")
	}
	var challenge protocol.SupervisorChallenge
	if err := json.Unmarshal(beginResponse.Result, &challenge); err != nil || challenge.InstanceID != string(c.instanceID) || len(challenge.Nonce) != model.RecoveryNonceBytes {
		return fmt.Errorf("runtime control returned an invalid challenge")
	}
	proof := model.RecoveryProof(c.privateKey, c.instanceID, model.SessionID(c.identity.SessionID), generation, challenge.Nonce,
		uint16(negotiated.Major), uint16(negotiated.Minor))
	body, _ := json.Marshal(protocol.SupervisorRegisterComplete{ChallengeID: challenge.ChallengeID, Proof: proof})
	if err := codec.Write(protocol.Request{ID: "control-complete", Type: "supervisor.control_complete", InstanceID: string(c.instanceID),
		SessionID: c.identity.SessionID, RuntimeGeneration: &generation, Body: body}); err != nil {
		return err
	}
	var completeResponse protocol.Response
	if err := codec.Read(&completeResponse); err != nil {
		return err
	}
	if err := validateResponse(completeResponse, "control-complete"); err != nil || completeResponse.Error != nil {
		return fmt.Errorf("runtime control authentication failed")
	}
	_ = conn.SetDeadline(time.Time{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	for {
		var request protocol.Request
		if err := codec.Read(&request); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		response := c.executeControl(ctx, controller, request)
		if err := codec.Write(response); err != nil {
			return err
		}
	}
}

func (c *SupervisorClient) executeControl(ctx context.Context, controller RuntimeController, request protocol.Request) protocol.Response {
	generation := c.identity.RuntimeGeneration
	if request.Validate() != nil || request.InstanceID != string(c.instanceID) || request.SessionID != c.identity.SessionID ||
		request.RuntimeGeneration == nil || *request.RuntimeGeneration != generation || request.OwnershipEpoch == nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: protocol.ErrInvalidArgument, Message: "invalid runtime control command"}}
	}
	var err error
	switch request.Type {
	case "supervisor.input":
		var input protocol.SupervisorInput
		if decodeErr := decodeStrict(request.Body, &input); decodeErr != nil || input.Sequence == 0 || len(input.Data) == 0 || len(input.Data) > 64<<10 {
			err = fmt.Errorf("invalid supervisor input")
		} else {
			err = controller.SubmitInput(ctx, duckruntime.InputFrame{Sequence: input.Sequence, Owner: input.Owner, OwnershipEpoch: *request.OwnershipEpoch,
				RuntimeGeneration: generation, Data: input.Data})
		}
	case "supervisor.resize":
		var resize protocol.SupervisorResize
		if decodeErr := decodeStrict(request.Body, &resize); decodeErr != nil {
			err = decodeErr
		} else {
			err = controller.Resize(resize.Rows, resize.Cols, *request.OwnershipEpoch, generation)
		}
	default:
		err = fmt.Errorf("unsupported runtime control command")
	}
	if err != nil {
		return protocol.Response{ID: request.ID, Error: &protocol.Error{Code: serviceMapError(err), Message: err.Error()}}
	}
	result, _ := json.Marshal(map[string]bool{"ok": true})
	return protocol.Response{ID: request.ID, Result: result}
}

func (c *SupervisorClient) Identity() protocol.SupervisorRegistered { return c.identity }
func (c *SupervisorClient) Close() error                            { return c.conn.Close() }

func (c *SupervisorClient) PublishOutput(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	for len(data) > 0 {
		length := len(data)
		if length > 64<<10 {
			length = 64 << 10
		}
		chunk := append([]byte(nil), data[:length]...)
		body, _ := json.Marshal(protocol.SupervisorOutput{Offset: c.nextOffset, Data: chunk})
		c.nextID++
		requestID := fmt.Sprintf("output-%d", c.nextID)
		generation := c.identity.RuntimeGeneration
		request := protocol.Request{ID: requestID, Type: "supervisor.output", InstanceID: string(c.instanceID), SessionID: c.identity.SessionID,
			RuntimeGeneration: &generation, Body: body}
		if err := c.codec.Write(request); err != nil {
			return err
		}
		var response protocol.Response
		if err := c.codec.Read(&response); err != nil {
			return err
		}
		if err := validateResponse(response, requestID); err != nil {
			return err
		}
		if response.Error != nil {
			return fmt.Errorf("publish supervisor output: %s", response.Error.Message)
		}
		var ack protocol.SupervisorOutputAck
		if err := json.Unmarshal(response.Result, &ack); err != nil || ack.Offset != c.nextOffset || ack.Length != uint64(length) {
			return fmt.Errorf("invalid supervisor output acknowledgement")
		}
		c.nextOffset += uint64(length)
		data = data[length:]
	}
	return nil
}

func (c *SupervisorClient) ForwardOutput(ctx context.Context, output *duckruntime.OutputHub) error {
	replay, stream, cancel, err := output.Subscribe(0, 64)
	if err != nil {
		return err
	}
	defer cancel()
	if replay.Gap {
		return fmt.Errorf("supervisor output replay begins with a gap")
	}
	if err := c.PublishOutput(replay.Data); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-stream:
			if !ok {
				return nil
			}
			if frame.Gap {
				return fmt.Errorf("supervisor output forwarding lost data")
			}
			if err := c.PublishOutput(frame.Data); err != nil {
				return err
			}
		}
	}
}

func validateResponse(response protocol.Response, expectedID string) error {
	if err := response.Validate(); err != nil {
		return err
	}
	if response.ID != expectedID {
		return fmt.Errorf("supervisor response ID mismatch")
	}
	return nil
}
