/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012-2014  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package openpgp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
	"gopkg.in/errgo.v1"
)

// Comparable time flag for "never expires"
var NeverExpires time.Time

var ErrMissingSignature = fmt.Errorf("Key material missing an expected signature")

func init() {
	t, err := time.Parse("2006-01-02 15:04:05 -0700", "9999-12-31 23:59:59 +0000")
	if err != nil {
		panic(err)
	}
	NeverExpires = t
}

/*
// Get the public key fingerprint as a hex string.
func Fingerprint(pubkey *packet.PublicKey) string {
	return hex.EncodeToString(pubkey.Fingerprint[:])
}

// Get the public key fingerprint as a hex string.
func FingerprintV3(pubkey *packet.PublicKeyV3) string {
	return hex.EncodeToString(pubkey.Fingerprint[:])
}
*/

func WritePackets(w io.Writer, root packetNode) error {
	for _, node := range root.contents() {
		op, err := newOpaquePacket(node.packet().Packet)
		if err != nil {
			return errgo.Mask(err)
		}
		err = op.Serialize(w)
		if err != nil {
			return errgo.Mask(err)
		}
	}
	return nil
}

func WriteArmoredPackets(w io.Writer, root packetNode) error {
	armw, err := armor.Encode(w, openpgp.PublicKeyType, nil)
	defer armw.Close()
	if err != nil {
		return errgo.Mask(err)
	}
	return WritePackets(armw, root)
}

type OpaqueKeyring struct {
	Packets      []*packet.OpaquePacket
	RFingerprint string
	Md5          string
	Sha256       string
	Error        error
	Position     int64
}

func (okr *OpaqueKeyring) setPosition(r io.Reader) {
	f, ok := r.(*os.File)
	if ok {
		pos, err := f.Seek(0, 1)
		if err == nil {
			okr.Position = pos
			return
		}
	}
	okr.Position = -1
}

func (ok *OpaqueKeyring) Parse() (*Pubkey, error) {
	var err error
	var pubkey *Pubkey
	var signablePacket signable
	for _, opkt := range ok.Packets {
		var badPacket *packet.OpaquePacket
		if opkt.Tag == 6 { //packet.PacketTypePublicKey:
			if pubkey != nil {
				return nil, errgo.Newf("multiple public keys in keyring")
			}
			if pubkey, err = ParsePubkey(opkt); err != nil {
				return nil, errgo.Notef(err, "invalid public key packet type")
			}
			signablePacket = pubkey
		} else if pubkey != nil {
			switch opkt.Tag {
			case 14: //packet.PacketTypePublicSubkey:
				signablePacket = nil
				subkey, err := ParseSubkey(opkt)
				if err != nil {
					badPacket = opkt
				} else {
					pubkey.Subkeys = append(pubkey.Subkeys, subkey)
					signablePacket = subkey
				}
			case 13: //packet.PacketTypeUserId:
				signablePacket = nil
				uid, err := ParseUserID(opkt, pubkey.UUID)
				if err != nil {
					badPacket = opkt
				} else {
					pubkey.UserIDs = append(pubkey.UserIDs, uid)
					signablePacket = uid
				}
			case 17: //packet.PacketTypeUserAttribute:
				signablePacket = nil
				uat, err := ParseUserAttribute(opkt, pubkey.UUID)
				if err != nil {
					badPacket = opkt
				} else {
					pubkey.UserAttributes = append(pubkey.UserAttributes, uat)
					signablePacket = uat
				}
			case 2: //packet.PacketTypeSignature:
				sig, err := ParseSignature(opkt, pubkey.UUID, signablePacket.uuid())
				if err != nil {
					badPacket = opkt
				} else if signablePacket == nil {
					badPacket = opkt
				} else {
					signablePacket.appendSignature(sig)
				}
			default:
				badPacket = opkt
			}

			if badPacket != nil {
				var badParent string
				if signablePacket != nil {
					badParent = signablePacket.uuid()
				} else {
					badParent = pubkey.uuid()
				}
				other, err := ParseOther(badPacket, badParent)
				if err != nil {
					return nil, errgo.Mask(err)
				}
				pubkey.Others = append(pubkey.Others, other)
			}
		}
	}
	if pubkey == nil {
		return nil, errgo.New("primary public key not found")
	}
	/*
		// Update the overall public key material digest.
		pubkey.updateDigests()
		// Validate signatures and wire-up relationships.
		// Also flags invalid key material but does not remove it.
		Resolve(pubkey)
	*/
	err = Deduplicate(pubkey)
	if err != nil {
		return nil, err
	}
	return pubkey, nil
}

type OpaqueKeyringChan chan *OpaqueKeyring

func ReadOpaqueKeyrings(r io.Reader) OpaqueKeyringChan {
	c := make(OpaqueKeyringChan)
	or := packet.NewOpaqueReader(r)
	go func() {
		defer close(c)
		var op *packet.OpaquePacket
		var err error
		var current *OpaqueKeyring
		for op, err = or.Next(); err == nil; op, err = or.Next() {
			switch op.Tag {
			case 6: //packet.PacketTypePublicKey:
				if current != nil {
					c <- current
					current = nil
				}
				current = new(OpaqueKeyring)
				current.setPosition(r)
				fallthrough
			case 13: //packet.PacketTypeUserId:
				fallthrough
			case 17: //packet.PacketTypeUserAttribute:
				fallthrough
			case 14: //packet.PacketTypePublicSubkey:
				fallthrough
			case 2: //packet.PacketTypeSignature:
				current.Packets = append(current.Packets, op)
			}
		}
		if err == io.EOF && current != nil {
			c <- current
		} else if err != nil {
			if current == nil {
				current = &OpaqueKeyring{}
			}
			current.Error = errgo.Mask(err)
			c <- current
		}
	}()
	return c
}

// SksDigest calculates a cumulative message digest on all
// OpenPGP packets for a given primary public key,
// using the same ordering as SKS, the Synchronizing Key Server.
// Use MD5 for matching digest values with SKS.
func SksDigest(key *Pubkey, h hash.Hash) (string, error) {
	var fail string
	var packets opaquePacketSlice
	for _, node := range key.contents() {
		op, err := newOpaquePacket(node.packet().Packet)
		if err != nil {
			return fail, errgo.Mask(err)
		}
		packets = append(packets, op)
	}
	if len(packets) == 0 {
		return fail, errgo.New("no packets found")
	}
	return sksDigestOpaque(packets, h), nil
}

func sksDigestOpaque(packets []*packet.OpaquePacket, h hash.Hash) string {
	sort.Sort(opaquePacketSlice(packets))
	for _, opkt := range packets {
		binary.Write(h, binary.BigEndian, int32(opkt.Tag))
		binary.Write(h, binary.BigEndian, int32(len(opkt.Contents)))
		h.Write(opkt.Contents)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type ReadKeyResult struct {
	*Pubkey
	Error error
}

type ReadKeyResults []*ReadKeyResult

func (r ReadKeyResults) GoodKeys() []*Pubkey {
	var result []*Pubkey
	for _, rkr := range r {
		if rkr.Error == nil {
			result = append(result, rkr.Pubkey)
		}
	}
	return result
}

type PubkeyChan chan *ReadKeyResult

func ErrReadKeys(msg string) *ReadKeyResult {
	return &ReadKeyResult{Error: fmt.Errorf(msg)}
}

/*
func (pubkey *Pubkey) updateDigests() {
	pubkey.MD5 = SksDigest(pubkey, md5.New())
	pubkey.SHA256 = SksDigest(pubkey, sha256.New())
}
*/

func ReadKeys(r io.Reader) PubkeyChan {
	c := make(PubkeyChan)
	go func() {
		defer close(c)
		for keyRead := range readKeys(r) {
			if keyRead.Error == nil {
				keyRead.Error = Deduplicate(keyRead.Pubkey)
			}
			c <- keyRead
		}
	}()
	return c
}

// Read one or more public keys from input.
func readKeys(r io.Reader) PubkeyChan {
	c := make(PubkeyChan)
	go func() {
		defer close(c)
		for opkr := range ReadOpaqueKeyrings(r) {
			pubkey, err := opkr.Parse()
			if err != nil {
				c <- &ReadKeyResult{Error: err}
			} else {
				c <- &ReadKeyResult{Pubkey: pubkey}
			}
		}
	}()
	return c
}
