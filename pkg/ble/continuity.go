package ble

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// AppleCompanyID is the Bluetooth SIG Company Identifier for Apple, Inc. (0x004C)
	AppleCompanyID uint16 = 0x004C

	// ContinuityTypeAirDrop is the Apple Continuity TLV type for AirDrop discovery (0x05)
	ContinuityTypeAirDrop uint8 = 0x05

	// ContinuityTypeNearbyInfo is the Apple Continuity TLV type for Nearby Info (0x10)
	ContinuityTypeNearbyInfo uint8 = 0x10

	// ContinuityTypeNearbyAction is the Apple Continuity TLV type for Nearby Action (0x0F)
	ContinuityTypeNearbyAction uint8 = 0x0F

	// AirDropPayloadLength is the standard 18-byte length of an AirDrop BLE advertisement TLV payload
	AirDropPayloadLength uint8 = 0x12
)

// AirDropFrame represents a decoded or constructible Apple Continuity AirDrop (0x05) BLE advertisement.
//
// Wire layout after Company ID (0x4C, 0x00):
//   Byte 0:      Type (0x05 = AirDrop)
//   Byte 1:      Length (0x12 = 18 bytes)
//   Bytes 2..9:  8 bytes of 0x00 padding
//   Byte 10:     Status / Version flag (typically 0x01)
//   Bytes 11..12: First 2 bytes of SHA-256(AppleID)
//   Bytes 13..14: First 2 bytes of SHA-256(Phone)
//   Bytes 15..16: First 2 bytes of SHA-256(Email)
//   Bytes 17..18: First 2 bytes of SHA-256(Email2)
//   Byte 19:     1 byte of 0x00 padding / Wi-Fi Aware capability bit (on iOS 26/27+)
type AirDropFrame struct {
	Status       uint8
	AppleIDHash  [2]byte
	PhoneHash    [2]byte
	EmailHash    [2]byte
	Email2Hash   [2]byte
	SupportsNAN  bool // iOS 26/27+ flag indicating Wi-Fi Aware (NAN) datapath capability
}

// DiscoveredBLEPeer represents an Apple device detected via BLE Continuity advertisements.
type DiscoveredBLEPeer struct {
	Address        string       `json:"address"`
	RSSI           int16        `json:"rssi"`
	DeviceCategory string       `json:"deviceCategory"` // e.g. "iPhone", "MacBook", "iPad", "Apple Device"
	HasAirDrop     bool         `json:"hasAirDrop"`
	AirDropFrame   *AirDropFrame `json:"airDropFrame,omitempty"`
	ActivityStatus string       `json:"activityStatus"`
	LastSeen       time.Time    `json:"lastSeen"`
	RawHex         string       `json:"rawHex"`
}

// ShortHash computes the first 2 bytes of SHA-256 for a normalized contact identifier
// (lowercase trimmed email or E.164 phone number), matching Apple's identity hash truncation.
func ShortHash(identifier string) [2]byte {
	normalized := strings.ToLower(strings.TrimSpace(identifier))
	if normalized == "" {
		return [2]byte{0x00, 0x00}
	}
	sum := sha256.Sum256([]byte(normalized))
	return [2]byte{sum[0], sum[1]}
}

// NewAirDropFrame creates an AirDrop BLE advertisement frame for the given contact identifiers.
// If all identifiers are empty, it uses anonymous/Everyone hashes (0x0000) which still wakes up
// nearby iOS/macOS sharingd daemons to listen on AWDL / Wi-Fi Aware / LAN.
func NewAirDropFrame(appleID, phone, email string, supportsNAN bool) *AirDropFrame {
	emailHash := ShortHash(email)
	return &AirDropFrame{
		Status:      0x01,
		AppleIDHash: ShortHash(appleID),
		PhoneHash:   ShortHash(phone),
		EmailHash:   emailHash,
		Email2Hash:  emailHash,
		SupportsNAN: supportsNAN,
	}
}

// EncodeManufacturerData encodes the AirDrop frame into the exact 20-byte payload
// following the 2-byte Apple Company ID (0x4C00), or 22 bytes including Company ID.
func (f *AirDropFrame) EncodeManufacturerData(includeCompanyID bool) []byte {
	tlv := make([]byte, 20)
	tlv[0] = ContinuityTypeAirDrop
	tlv[1] = AirDropPayloadLength
	// tlv[2..9] are 8 bytes of 0x00 padding
	tlv[10] = f.Status
	copy(tlv[11:13], f.AppleIDHash[:])
	copy(tlv[13:15], f.PhoneHash[:])
	copy(tlv[15:17], f.EmailHash[:])
	copy(tlv[17:19], f.Email2Hash[:])
	if f.SupportsNAN {
		tlv[19] = 0x01 // Indicate Wi-Fi Aware / NAN capability alongside AWDL
	} else {
		tlv[19] = 0x00
	}

	if !includeCompanyID {
		return tlv
	}

	full := make([]byte, 22)
	binary.LittleEndian.PutUint16(full[0:2], AppleCompanyID)
	copy(full[2:], tlv)
	return full
}

// ParseAppleManufacturerData inspects raw manufacturer-specific BLE data (excluding or including 0x4C00)
// and extracts Apple Continuity TLVs (AirDrop 0x05 and Nearby Info 0x10).
func ParseAppleManufacturerData(addr string, rssi int16, data []byte) (*DiscoveredBLEPeer, error) {
	payload := data
	if len(payload) >= 2 && binary.LittleEndian.Uint16(payload[0:2]) == AppleCompanyID {
		payload = payload[2:]
	}
	if len(payload) < 2 {
		return nil, fmt.Errorf("continuity payload too short (%d bytes)", len(payload))
	}

	peer := &DiscoveredBLEPeer{
		Address:        addr,
		RSSI:           rssi,
		DeviceCategory: "Apple Device",
		ActivityStatus: "Nearby",
		LastSeen:       time.Now(),
		RawHex:         hex.EncodeToString(data),
	}

	offset := 0
	foundAppleTLV := false
	for offset+2 <= len(payload) {
		tlvType := payload[offset]
		tlvLen := int(payload[offset+1])
		offset += 2

		if offset+tlvLen > len(payload) {
			// Truncated TLV
			break
		}
		tlvVal := payload[offset : offset+tlvLen]
		offset += tlvLen

		switch tlvType {
		case ContinuityTypeAirDrop:
			foundAppleTLV = true
			peer.HasAirDrop = true
			peer.ActivityStatus = "AirDrop Active"
			if len(tlvVal) >= 18 {
				frame := &AirDropFrame{
					Status:      tlvVal[8],
					SupportsNAN: tlvVal[17] != 0x00,
				}
				copy(frame.AppleIDHash[:], tlvVal[9:11])
				copy(frame.PhoneHash[:], tlvVal[11:13])
				copy(frame.EmailHash[:], tlvVal[13:15])
				copy(frame.Email2Hash[:], tlvVal[15:17])
				peer.AirDropFrame = frame
			}

		case ContinuityTypeNearbyInfo:
			foundAppleTLV = true
			if len(tlvVal) >= 2 {
				activity := tlvVal[0] & 0x0F
				flags := tlvVal[1]
				peer.DeviceCategory = guessAppleDeviceCategory(tlvVal[0], flags)
				peer.ActivityStatus = decodeNearbyActivity(activity)
			}

		case ContinuityTypeNearbyAction:
			foundAppleTLV = true
			peer.ActivityStatus = "Sharing / Continuity Action"
		}
	}

	if !foundAppleTLV {
		return nil, fmt.Errorf("no recognized Continuity TLV found")
	}

	return peer, nil
}

func guessAppleDeviceCategory(statusByte, flagsByte uint8) string {
	// Continuity NearbyInfo upper nibble and flags give hints about iOS vs macOS state
	if (flagsByte & 0x40) != 0 {
		return "iPhone / iPad"
	}
	if (statusByte & 0x80) != 0 {
		return "MacBook / Mac"
	}
	return "Apple Device"
}

func decodeNearbyActivity(code uint8) string {
	switch code {
	case 0x00:
		return "Idle"
	case 0x01:
		return "Idle (Screen Off)"
	case 0x03:
		return "Active"
	case 0x05:
		return "Audio Playing"
	case 0x07:
		return "Screen On (Unlocked)"
	case 0x09:
		return "Screen Sharing / Video"
	case 0x0A:
		return "Watch On Wrist"
	case 0x0B:
		return "Recent Interaction"
	default:
		return "Active Nearby"
	}
}
