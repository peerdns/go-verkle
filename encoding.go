// This is free and unencumbered software released into the public domain.
//
// Anyone is free to copy, modify, publish, use, compile, sell, or
// distribute this software, either in source code form or as a compiled
// binary, for any purpose, commercial or non-commercial, and by any
// means.
//
// In jurisdictions that recognize copyright laws, the author or authors
// of this software dedicate any and all copyright interest in the
// software to the public domain. We make this dedication for the benefit
// of the public at large and to the detriment of our heirs and
// successors. We intend this dedication to be an overt act of
// relinquishment in perpetuity of all present and future rights to this
// software under copyright law.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
// EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
// MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
// IN NO EVENT SHALL THE AUTHORS BE LIABLE FOR ANY CLAIM, DAMAGES OR
// OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
// ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
// OTHER DEALINGS IN THE SOFTWARE.
//
// For more information, please refer to <https://unlicense.org>

package verkle

import (
	"errors"
	"fmt"

	"github.com/crate-crypto/go-ipa/banderwagon"
)

var (
	ErrInvalidNodeEncoding = errors.New("invalid node encoding")

	mask = [8]byte{0x80, 0x40, 0x20, 0x10, 0x8, 0x4, 0x2, 0x1}
)

const (
	nodeTypeSize = 1
	bitlistSize  = NodeWidth / 8

	// Shared between internal and leaf nodes.
	nodeTypeOffset = 0

	// Internal nodes offsets.
	internalBitlistOffset    = nodeTypeOffset + nodeTypeSize
	internalCommitmentOffset = internalBitlistOffset + bitlistSize

	// Leaf node offsets.
	leafStemOffset         = nodeTypeOffset + nodeTypeSize
	leafBitlistOffset      = leafStemOffset + StemSize
	leafCommitmentOffset   = leafBitlistOffset + bitlistSize
	leafC1CommitmentOffset = leafCommitmentOffset + banderwagon.UncompressedSize
	leafC2CommitmentOffset = leafC1CommitmentOffset + banderwagon.UncompressedSize
	leafChildrenOffset     = leafC2CommitmentOffset + banderwagon.UncompressedSize
	leafBasicDataSize      = 32
	leafSlotSize           = 32
	leafValueIndexSize     = 1
	singleSlotLeafSize     = nodeTypeSize + StemSize + 2*banderwagon.UncompressedSize + leafValueIndexSize + leafSlotSize
	eoaLeafSize            = nodeTypeSize + StemSize + 2*banderwagon.UncompressedSize + leafBasicDataSize
)

func bit(bitlist []byte, nr int) bool {
	if len(bitlist)*8 <= nr {
		return false
	}
	return bitlist[nr/8]&mask[nr%8] != 0
}

var errSerializedPayloadTooShort = errors.New("verkle payload is too short")

// ParseNode deserializes a node into its proper VerkleNode instance.
// The serialized bytes have the format:
// - Internal nodes:   <nodeType><bitlist><commitment>
// - Leaf nodes:       <nodeType><stem><bitlist><comm><c1comm><c2comm><children...>
// - EoA nodes:        <nodeType><stem><comm><c1comm><balance><nonce>
// - single slot node: <nodeType><stem><comm><cncomm><leaf index><slot>
func ParseNode(serializedNode []byte, depth byte) (VerkleNode, error) {
	// Check that the length of the serialized node is at least the smallest possible serialized node.
	if len(serializedNode) < nodeTypeSize+banderwagon.UncompressedSize {
		return nil, errSerializedPayloadTooShort
	}

	nodeType := serializedNode[0]
	fmt.Printf("ParseNode: nodeType=%d, serializedNode=%x\n", nodeType, serializedNode)

	switch nodeType {
	case leafType:
		return parseLeafNode(serializedNode, depth)
	case internalType:
		return CreateInternalNode(serializedNode[internalBitlistOffset:internalCommitmentOffset], serializedNode[internalCommitmentOffset:], depth)
	case eoAccountType:
		return parseEoAccountNode(serializedNode, depth)
	case singleSlotType:
		return parseSingleSlotNode(serializedNode, depth)
	default:
		return nil, ErrInvalidNodeEncoding
	}
}

func parseLeafNode(serialized []byte, depth byte) (VerkleNode, error) {
	// Ensure that we have enough data for the stem
	stemEnd := leafStemOffset + StemSize
	if len(serialized) < stemEnd {
		return nil, fmt.Errorf("serialized data too short to contain stem")
	}
	stem := make([]byte, StemSize)
	copy(stem, serialized[leafStemOffset:stemEnd])

	// Ensure that we have enough data for the bitlist
	bitlistEnd := leafBitlistOffset + bitlistSize
	if len(serialized) < bitlistEnd {
		return nil, fmt.Errorf("serialized data too short to contain bitlist")
	}
	bitlist := serialized[leafBitlistOffset:bitlistEnd]

	// Ensure that we have enough data for the commitments
	commitmentEnd := leafC2CommitmentOffset + banderwagon.UncompressedSize
	if len(serialized) < commitmentEnd {
		return nil, fmt.Errorf("serialized data too short to contain commitments")
	}
	commitmentBytes := serialized[leafCommitmentOffset : leafCommitmentOffset+banderwagon.UncompressedSize]
	c1Bytes := serialized[leafC1CommitmentOffset : leafC1CommitmentOffset+banderwagon.UncompressedSize]
	c2Bytes := serialized[leafC2CommitmentOffset : leafC2CommitmentOffset+banderwagon.UncompressedSize]

	// Initialize the leaf node
	var values [NodeWidth][]byte
	ln := NewLeafNodeWithNoComms(stem, values[:])
	ln.setDepth(depth)

	// Set commitments
	ln.commitment = new(Point)
	if err := ln.commitment.SetBytesUncompressed(commitmentBytes, true); err != nil {
		return nil, fmt.Errorf("setting commitment: %w", err)
	}

	ln.c1 = new(Point)
	if err := ln.c1.SetBytesUncompressed(c1Bytes, true); err != nil {
		return nil, fmt.Errorf("setting c1 commitment: %w", err)
	}

	ln.c2 = new(Point)
	if err := ln.c2.SetBytesUncompressed(c2Bytes, true); err != nil {
		return nil, fmt.Errorf("setting c2 commitment: %w", err)
	}

	// Now parse the children
	offset := leafChildrenOffset
	for i := 0; i < NodeWidth; i++ {
		if bit(bitlist, i) {
			if offset+LeafValueSize > len(serialized) {
				return nil, fmt.Errorf("not enough data to read value at index %d", i)
			}
			ln.values[i] = serialized[offset : offset+LeafValueSize]
			offset += LeafValueSize
		}
	}

	return ln, nil
}

func parseEoAccountNode(serialized []byte, depth byte) (VerkleNode, error) {
	var values [NodeWidth][]byte
	offset := leafStemOffset + StemSize + 2*banderwagon.UncompressedSize
	values[0] = serialized[offset : offset+leafBasicDataSize] // basic data
	values[1] = EmptyCodeHash[:]
	ln := NewLeafNodeWithNoComms(serialized[leafStemOffset:leafStemOffset+StemSize], values[:])
	ln.setDepth(depth)
	ln.c1 = new(Point)
	if err := ln.c1.SetBytesUncompressed(serialized[leafStemOffset+StemSize:leafStemOffset+StemSize+banderwagon.UncompressedSize], true); err != nil {
		return nil, fmt.Errorf("error setting leaf C1 commitment: %w", err)
	}
	ln.c2 = &banderwagon.Identity
	ln.commitment = new(Point)
	if err := ln.commitment.SetBytesUncompressed(serialized[leafStemOffset+StemSize+banderwagon.UncompressedSize:leafStemOffset+StemSize+banderwagon.UncompressedSize*2], true); err != nil {
		return nil, fmt.Errorf("error setting leaf root commitment: %w", err)
	}
	return ln, nil
}

func parseSingleSlotNode(serialized []byte, depth byte) (VerkleNode, error) {
	var values [NodeWidth][]byte
	offset := leafStemOffset
	ln := NewLeafNodeWithNoComms(serialized[offset:offset+StemSize], values[:])
	offset += StemSize
	cnCommBytes := serialized[offset : offset+banderwagon.UncompressedSize]
	offset += banderwagon.UncompressedSize
	rootCommBytes := serialized[offset : offset+banderwagon.UncompressedSize]
	offset += banderwagon.UncompressedSize
	idx := serialized[offset]
	offset += leafValueIndexSize
	values[idx] = serialized[offset : offset+leafSlotSize] // copy slot
	ln.setDepth(depth)
	if idx < 128 {
		ln.c1 = new(Point)
		if err := ln.c1.SetBytesUncompressed(cnCommBytes, true); err != nil {
			return nil, fmt.Errorf("error setting leaf C1 commitment: %w", err)
		}
		ln.c2 = &banderwagon.Identity
	} else {
		ln.c2 = new(Point)
		if err := ln.c2.SetBytesUncompressed(cnCommBytes, true); err != nil {
			return nil, fmt.Errorf("error setting leaf C2 commitment: %w", err)
		}
		ln.c1 = &banderwagon.Identity
	}
	ln.commitment = new(Point)
	if err := ln.commitment.SetBytesUncompressed(rootCommBytes, true); err != nil {
		return nil, fmt.Errorf("error setting leaf root commitment: %w", err)
	}
	return ln, nil
}

func CreateInternalNode(bitlist []byte, raw []byte, depth byte) (*InternalNode, error) {
	// GetTreeConfig caches computation result, hence
	// this op has low overhead
	node := new(InternalNode)

	if len(bitlist) != bitlistSize {
		return nil, ErrInvalidNodeEncoding
	}

	// Create a HashNode placeholder for all values
	// corresponding to a set bit.
	node.children = make([]VerkleNode, NodeWidth)
	for i, b := range bitlist {
		for j := 0; j < 8; j++ {
			if b&mask[j] != 0 {
				node.children[8*i+j] = HashedNode{}
			} else {

				node.children[8*i+j] = Empty(struct{}{})
			}
		}
	}
	node.depth = depth
	if len(raw) != banderwagon.UncompressedSize {
		return nil, ErrInvalidNodeEncoding
	}

	node.commitment = new(Point)
	if err := node.commitment.SetBytesUncompressed(raw, true); err != nil {
		return nil, fmt.Errorf("setting commitment: %w", err)
	}
	return node, nil
}
