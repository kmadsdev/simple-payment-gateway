// Package pix builds and parses a simulated Pix "copy and paste" payload.
//
// The layout follows the EMV merchant-presented QR code style (ID, length,
// value fields and a CRC16 at the end), but the account information uses
// the identifier "example.simulated.pix". No bank app accepts it. It exists
// so the checkout flow has a realistic payload to show and to verify.
package pix

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kmadsdev/simple-payment-gateway/internal/money"
)

// SimulatedGUI marks the payload as fake.
const SimulatedGUI = "example.simulated.pix"

// ErrInvalidPayload is returned by Parse.
var ErrInvalidPayload = errors.New("invalid pix payload")

// Payload holds the fields the gateway reads back.
type Payload struct {
	TxID         string
	Amount       money.Amount
	MerchantName string
	City         string
}

func field(id, value string) string {
	return fmt.Sprintf("%s%02d%s", id, len(value), value)
}

// Build returns the copy-and-paste string for one charge.
// txid must be 1..25 alphanumeric characters.
func Build(p Payload) (string, error) {
	if p.TxID == "" || len(p.TxID) > 25 || !alnum(p.TxID) {
		return "", fmt.Errorf("%w: txid", ErrInvalidPayload)
	}
	if !p.Amount.Positive() {
		return "", fmt.Errorf("%w: amount", ErrInvalidPayload)
	}
	name := truncate(p.MerchantName, 25)
	city := truncate(p.City, 15)
	var b strings.Builder
	b.WriteString(field("00", "01"))
	b.WriteString(field("26", field("00", SimulatedGUI)+field("01", "simulated-key")))
	b.WriteString(field("52", "0000"))
	b.WriteString(field("53", "986")) // ISO 4217 numeric code for BRL
	b.WriteString(field("54", money.Format(p.Amount)))
	b.WriteString(field("58", "BR"))
	b.WriteString(field("59", name))
	b.WriteString(field("60", city))
	b.WriteString(field("62", field("05", p.TxID)))
	b.WriteString("6304")
	return b.String() + fmt.Sprintf("%04X", CRC16(b.String())), nil
}

// Parse verifies the CRC and reads the fields back.
func Parse(s string) (Payload, error) {
	if len(s) < 8 || s[len(s)-8:len(s)-4] != "6304" {
		return Payload{}, ErrInvalidPayload
	}
	want := fmt.Sprintf("%04X", CRC16(s[:len(s)-4]))
	if s[len(s)-4:] != want {
		return Payload{}, fmt.Errorf("%w: crc mismatch", ErrInvalidPayload)
	}
	fields, err := tlv(s[:len(s)-8])
	if err != nil {
		return Payload{}, err
	}
	acct, err := tlv(fields["26"])
	if err != nil || acct["00"] != SimulatedGUI {
		return Payload{}, fmt.Errorf("%w: not a simulated payload", ErrInvalidPayload)
	}
	add, err := tlv(fields["62"])
	if err != nil {
		return Payload{}, err
	}
	amount, err := money.Parse(fields["54"])
	if err != nil {
		return Payload{}, fmt.Errorf("%w: amount", ErrInvalidPayload)
	}
	return Payload{TxID: add["05"], Amount: amount, MerchantName: fields["59"], City: fields["60"]}, nil
}

func tlv(s string) (map[string]string, error) {
	out := map[string]string{}
	for i := 0; i < len(s); {
		if i+4 > len(s) {
			return nil, ErrInvalidPayload
		}
		id := s[i : i+2]
		n, err := strconv.Atoi(s[i+2 : i+4])
		if err != nil || n < 0 || i+4+n > len(s) {
			return nil, ErrInvalidPayload
		}
		out[id] = s[i+4 : i+4+n]
		i += 4 + n
	}
	return out, nil
}

// CRC16 is CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF), the checksum the
// EMV QR layout uses.
func CRC16(s string) uint16 {
	crc := uint16(0xFFFF)
	for i := 0; i < len(s); i++ {
		crc ^= uint16(s[i]) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func alnum(s string) bool {
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !ok {
			return false
		}
	}
	return true
}

// truncate keeps ASCII letters, digits and spaces, then cuts to n bytes.
func truncate(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "STORE"
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}
