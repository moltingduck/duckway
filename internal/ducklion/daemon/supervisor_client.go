package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/bridge"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type SupervisorClient struct {
	conn       *net.UnixConn
	codec      *bridge.Codec
	identity   protocol.SupervisorRegistered
	instanceID model.InstanceID
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
		Principal: string(sessionID), Capabilities: []string{"supervisor_recovery"}}
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
		negotiated.Principal != string(sessionID) || !hasCapability(negotiated.Capabilities, "supervisor_recovery") {
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

func validateResponse(response protocol.Response, expectedID string) error {
	if err := response.Validate(); err != nil {
		return err
	}
	if response.ID != expectedID {
		return fmt.Errorf("supervisor response ID mismatch")
	}
	return nil
}
