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
	return &SupervisorClient{conn: conn, codec: codec, identity: identity, instanceID: instanceID}, nil
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
