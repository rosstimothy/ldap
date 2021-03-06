package ldap

import (
	"net"
	"strings"
	"errors"
	"fmt"

	"github.com/mellium/sasl"
	"gopkg.in/asn1-ber.v1"
)

// SimpleBindRequest represents a username/password bind operation
type SimpleBindRequest struct {
	// Username is the name of the Directory object that the client wishes to bind as
	Username string
	// Password is the credentials to bind with
	Password string
	// Controls are optional controls to send with the bind request
	Controls []Control
	// AllowEmptyPassword sets whether the client allows binding with an empty password
	// (normally used for unauthenticated bind).
	AllowEmptyPassword bool
}

// SimpleBindResult contains the response from the server
type SimpleBindResult struct {
	Controls []Control
}

// NewSimpleBindRequest returns a bind request
func NewSimpleBindRequest(username string, password string, controls []Control) *SimpleBindRequest {
	return &SimpleBindRequest{
		Username:           username,
		Password:           password,
		Controls:           controls,
		AllowEmptyPassword: false,
	}
}

func (bindRequest *SimpleBindRequest) encode() *ber.Packet {
	request := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationBindRequest, nil, "Bind Request")
	request.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, 3, "Version"))
	request.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, bindRequest.Username, "User Name"))
	request.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 0, bindRequest.Password, "Password"))

	return request
}

// SimpleBind performs the simple bind operation defined in the given request
func (l *Conn) SimpleBind(simpleBindRequest *SimpleBindRequest) (*SimpleBindResult, error) {
	if simpleBindRequest.Password == "" && !simpleBindRequest.AllowEmptyPassword {
		return nil, NewError(ErrorEmptyPassword, errors.New("ldap: empty password not allowed by the client"))
	}

	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Request")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, l.nextMessageID(), "MessageID"))
	encodedBindRequest := simpleBindRequest.encode()
	packet.AppendChild(encodedBindRequest)
	if len(simpleBindRequest.Controls) > 0 {
		packet.AppendChild(encodeControls(simpleBindRequest.Controls))
	}

	if l.Debug {
		ber.PrintPacket(packet)
	}

	msgCtx, err := l.sendMessage(packet)
	if err != nil {
		return nil, err
	}
	defer l.finishMessage(msgCtx)

	packetResponse, ok := <-msgCtx.responses
	if !ok {
		return nil, NewError(ErrorNetwork, errors.New("ldap: response channel closed"))
	}
	packet, err = packetResponse.ReadPacket()
	l.Debug.Printf("%d: got response %p", msgCtx.id, packet)
	if err != nil {
		return nil, err
	}

	if l.Debug {
		if err = addLDAPDescriptions(packet); err != nil {
			return nil, err
		}
		ber.PrintPacket(packet)
	}

	result := &SimpleBindResult{
		Controls: make([]Control, 0),
	}

	if len(packet.Children) == 3 {
		for _, child := range packet.Children[2].Children {
			decodedChild, decodeErr := DecodeControl(child)
			if decodeErr != nil {
				return nil, fmt.Errorf("failed to decode child control: %s", decodeErr)
			}
			result.Controls = append(result.Controls, decodedChild)
		}
	}

	err = GetLDAPError(packet)
	return result, err
}

// Bind performs a bind with the given username and password.
//
// It does not allow unauthenticated bind (i.e. empty password). Use the UnauthenticatedBind method
// for that.
func (l *Conn) Bind(username, password string) error {
	req := &SimpleBindRequest{
		Username:           username,
		Password:           password,
		AllowEmptyPassword: false,
	}
	_, err := l.SimpleBind(req)
	return err
}

// UnauthenticatedBind performs an unauthenticated bind.
//
// A username may be provided for trace (e.g. logging) purpose only, but it is normally not
// authenticated or otherwise validated by the LDAP server.
//
// See https://tools.ietf.org/html/rfc4513#section-5.1.2 .
// See https://tools.ietf.org/html/rfc4513#section-6.3.1 .
func (l *Conn) UnauthenticatedBind(username string) error {
	req := &SimpleBindRequest{
		Username:           username,
		Password:           "",
		AllowEmptyPassword: true,
	}
	_, err := l.SimpleBind(req)
	return err
}

// SaslBindResult contains the response from the server
type SaslBindResult struct {
	Credentials string
	Controls    []Control
}

func (l *Conn) spn() (string, error) {
	addr := l.conn.RemoteAddr().String()
	idx := strings.LastIndex(addr, ":")
	if idx != -1 {
		addr = addr[:idx]
	}
	l.Debug.Printf("connected to %s", addr)
	names, err := net.LookupAddr(addr)
	if err != nil {
		l.Debug.Printf("lookup failed: %v", err)
		return "", errors.New("unable to lookup host")
	}
	if len(names) <= 0 {
		return "", errors.New("no host name found")
	}

	host := strings.TrimSuffix(names[0], ".")

	return fmt.Sprintf("ldap/%s", host), nil
}

//SaslBind performs an ldap sasl bind
func (l *Conn) SaslBind() error {
	spn, err := l.spn()
	if err != nil {
		return err
	}
	l.Debug.Printf("SPN: %s", spn)
	client := sasl.NewClient(sasl.NTLM(spn))

	more, resp, err := client.Step(nil)
	for {
		l.Debug.Printf("More: %t, Resp: %q, Err %v", more, resp, err)
		if err != nil {
			return err
		}
		if !more {
			break
		}

		res, err := l.sendSaslBindRequest(resp)
		if err != nil {
			if e, ok := err.(*Error); ok {
				if e.ResultCode != LDAPResultSaslBindInProgress {
					return errors.New("authentication failed")
				}
			}

			l.Debug.Printf("bind response received, %v", err)
		}

		more, resp, err = client.Step([]byte(res.Credentials))
	}

	l.Debug.Printf("all done binding")
	return nil
}

func (l *Conn) sendSaslBindRequest(credentials []byte) (*SaslBindResult, error) {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Request")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, l.nextMessageID(), "MessageID"))

	request := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ApplicationBindRequest, nil, "Bind Request")
	request.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, 3, "Version"))
	request.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "Name"))

	sasl := ber.Encode(ber.ClassContext, ber.TypeConstructed, 3, nil, "SASL")
	sasl.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "GSS-SPNEGO", "Mechanism"))
	if len(credentials) > 0 {
		sasl.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, string(credentials), "Credentials"))
	}
	request.AppendChild(sasl)
	packet.AppendChild(request)

	if l.Debug {
		ber.PrintPacket(packet)
	}

	msgCtx, err := l.sendMessage(packet)
	if err != nil {
		return nil, err
	}
	defer l.finishMessage(msgCtx)

	packetResponse, ok := <-msgCtx.responses
	if !ok {
		return nil, NewError(ErrorNetwork, errors.New("ldap: response channel closed"))
	}
	packet, err = packetResponse.ReadPacket()
	l.Debug.Printf("%d: got response %p", msgCtx.id, packet)
	if err != nil {
		return nil, err
	}

	if l.Debug {
		if err = addLDAPDescriptions(packet); err != nil {
			return nil, err
		}
		ber.PrintPacket(packet)
	}

	var b strings.Builder
	if len(packet.Children[1].Children) > 3 {
		b.Write(packet.Children[1].Children[3].Data.Bytes())
	}
	result := &SaslBindResult{
		Credentials: b.String(),
		Controls: make([]Control, 0),
	}

	if len(packet.Children) == 3 {
		for _, child := range packet.Children[2].Children {
			decodedChild, decodeErr := DecodeControl(child)
			if decodeErr != nil {
				return nil, fmt.Errorf("failed to decode child control: %s", decodeErr)
			}
			result.Controls = append(result.Controls, decodedChild)
		}
	}

	err = GetLDAPError(packet)
	return result, err
}