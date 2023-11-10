// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package art provides a routing table that implements the Allotment Routing
// Table (ART) algorithm by Donald Knuth, as described in the paper by Yoichi
// Hariguchi.
//
// ART outperforms the traditional radix tree implementations for route lookups,
// insertions, and deletions.
//
// For more information, see Yoichi Hariguchi's paper:
// https://cseweb.ucsd.edu//~varghese/TEACH/cs228/artlookup.pdf
package art

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"strings"
	"sync"
)

const (
	debugInsert   = false
	debugDelete   = false
	debugOverlaps = false
)

// Table is an IPv4 and IPv6 routing table.
type Table[T any] struct {
	v4       strideTable[T]
	v6       strideTable[T]
	initOnce sync.Once
}

func (t *Table[T]) init() {
	t.initOnce.Do(func() {
		t.v4.prefix = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
		t.v6.prefix = netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	})
}

func (t *Table[T]) tableForAddr(addr netip.Addr) *strideTable[T] {
	if addr.Is6() {
		return &t.v6
	}
	return &t.v4
}

// Get does a route lookup for addr and returns the associated value, or nil if
// no route matched.
func (t *Table[T]) Get(addr netip.Addr) (ret T, ok bool) {
	t.init()

	// Ideally we would use addr.AsSlice here, but AsSlice is just
	// barely complex enough that it can't be inlined, and that in
	// turn causes the slice to escape to the heap. Using As16 and
	// manual slicing here helps the compiler keep Get alloc-free.
	st := t.tableForAddr(addr)
	rawAddr := addr.As16()
	bs := rawAddr[:]
	if addr.Is4() {
		bs = bs[12:]
	}

	i := 0
	// With path compression, we might skip over some address bits while walking
	// to a strideTable leaf. This means the leaf answer we find might not be
	// correct, because path compression took us down the wrong subtree. When
	// that happens, we have to backtrack and figure out which most specific
	// route further up the tree is relevant to addr, and return that.
	//
	// So, as we walk down the stride tables, each time we find a non-nil route
	// result, we have to remember it and the associated strideTable prefix.
	//
	// We could also deal with this edge case of path compression by checking
	// the strideTable prefix on each table as we descend, but that means we
	// have to pay N prefix.Contains checks on every route lookup (where N is
	// the number of strideTables in the path), rather than only paying M prefix
	// comparisons in the edge case (where M is the number of strideTables in
	// the path with a non-nil route of their own).
	const maxDepth = 16
	type prefixAndRoute struct {
		prefix netip.Prefix
		route  T
	}
	strideMatch := make([]prefixAndRoute, 0, maxDepth)
findLeaf:
	for {
		rt, rtOK, child := st.getValAndChild(bs[i])
		if rtOK {
			// This strideTable contains a route that may be relevant to our
			// search, remember it.
			strideMatch = append(strideMatch, prefixAndRoute{st.prefix, rt})
		}
		if child == nil {
			// No sub-routes further down, the last thing we recorded
			// in strideRoutes is tentatively the result, barring
			// misdirection from path compression.
			break findLeaf
		}
		st = child
		// Path compression means we may be skipping over some intermediate
		// tables. We have to skip forward to whatever depth st now references.
		i = st.prefix.Bits() / 8
	}

	// Walk backwards through the hits we recorded in strideRoutes and
	// stridePrefixes, returning the first one whose subtree matches addr.
	//
	// In the common case where path compression did not mislead us, we'll
	// return on the first loop iteration because the last route we recorded was
	// the correct most-specific route.
	for i := len(strideMatch) - 1; i >= 0; i-- {
		if m := strideMatch[i]; m.prefix.Contains(addr) {
			return m.route, true
		}
	}

	// We either found no route hits at all (both previous loops terminated
	// immediately), or we went on a wild goose chase down a compressed path for
	// the wrong prefix, and also found no usable routes on the way back up to
	// the root. This is a miss.
	return ret, false
}

// Insert adds pfx to the table, with value val.
// If pfx is already present in the table, its value is set to val.
func (t *Table[T]) Insert(pfx netip.Prefix, val T) {
	t.init()

	// The standard library doesn't enforce normalized prefixes (where
	// the non-prefix bits are all zero). These algorithms require
	// normalized prefixes, so do it upfront.
	pfx = pfx.Masked()

	if debugInsert {
		defer func() {
			fmt.Printf("%s", t.debugSummary())
		}()
		fmt.Printf("\ninsert: start pfx=%s\n", pfx)
	}

	st := t.tableForAddr(pfx.Addr())

	// This algorithm is full of off-by-one headaches that boil down
	// to the fact that pfx.Bits() has (2^n)+1 values, rather than
	// just 2^n. For example, an IPv4 prefix length can be 0 through
	// 32, which is 33 values.
	//
	// This extra possible value creates a lot of problems as we do
	// bits and bytes math to traverse strideTables below. So, we
	// treat the default route 0/0 specially here, that way the rest
	// of the logic goes back to having 2^n values to reason about,
	// which can be done in a nice and regular fashion with no edge
	// cases.
	if pfx.Bits() == 0 {
		if debugInsert {
			fmt.Printf("insert: default route\n")
		}
		st.insert(0, 0, val)
		return
	}

	// No matter what we do as we traverse strideTables, our final
	// action will be to insert the last 1-8 bits of pfx into a
	// strideTable somewhere.
	//
	// We calculate upfront the byte position of the end of the
	// prefix; the number of bits within that byte that contain prefix
	// data; and the prefix of the strideTable into which we'll
	// eventually insert.
	//
	// We need this in a couple different branches of the code below,
	// and because the possible values are 1-indexed (1 through 32 for
	// ipv4, 1 through 128 for ipv6), the math is very slightly
	// unusual to account for the off-by-one indexing. Do it once up
	// here, with this large comment, rather than reproduce the subtle
	// math in multiple places further down.
	finalByteIdx := (pfx.Bits() - 1) / 8
	finalBits := pfx.Bits() - (finalByteIdx * 8)
	finalStridePrefix, err := pfx.Addr().Prefix(finalByteIdx * 8)
	if err != nil {
		panic(fmt.Sprintf("invalid prefix requested: %s/%d", pfx.Addr(), finalByteIdx*8))
	}
	if debugInsert {
		fmt.Printf("insert: finalByteIdx=%d finalBits=%d finalStridePrefix=%s\n", finalByteIdx, finalBits, finalStridePrefix)
	}

	// The strideTable we want to insert into is potentially at the
	// end of a chain of strideTables, each one encoding 8 bits of the
	// prefix.
	//
	// We're expecting to walk down a path of tables, although with
	// prefix compression we may end up skipping some links in the
	// chain, or taking wrong turns and having to course correct.
	//
	// As we walk down the tree, byteIdx is the byte of bs we're
	// currently examining to choose our next step, and numBits is the
	// number of bits that remain in pfx, starting with the byte at
	// byteIdx inclusive.
	bs := pfx.Addr().AsSlice()
	byteIdx := 0
	numBits := pfx.Bits()
	for {
		if debugInsert {
			fmt.Printf("insert: loop byteIdx=%d numBits=%d st.prefix=%s\n", byteIdx, numBits, st.prefix)
		}
		if numBits <= 8 {
			if debugInsert {
				fmt.Printf("insert: existing leaf st.prefix=%s addr=%d/%d\n", st.prefix, bs[finalByteIdx], finalBits)
			}
			// We've reached the end of the prefix, whichever
			// strideTable we're looking at now is the place where we
			// need to insert.
			st.insert(bs[finalByteIdx], finalBits, val)
			return
		}

		// Otherwise, we need to go down at least one more level of
		// strideTables. With prefix compression, each level of
		// descent can have one of three outcomes: we find a place
		// where prefix compression is possible; a place where prefix
		// compression made us take a "wrong turn"; or a point along
		// our intended path that we have to keep following.
		child, created := st.getOrCreateChild(bs[byteIdx])
		switch {
		case created:
			// The subtree we need for pfx doesn't exist yet. The rest
			// of the path, if we were to create it, will consist of a
			// bunch of strideTables with a single child each. We can
			// use path compression to elide those intermediates, and
			// jump straight to the final strideTable that hosts this
			// prefix.
			child.prefix = finalStridePrefix
			child.insert(bs[finalByteIdx], finalBits, val)
			if debugInsert {
				fmt.Printf("insert: new leaf st.prefix=%s child.prefix=%s addr=%d/%d\n", st.prefix, child.prefix, bs[finalByteIdx], finalBits)
			}
			return
		case !prefixStrictlyContains(child.prefix, pfx):
			// child already exists, but its prefix does not contain
			// our destination. This means that the path between st
			// and child was compressed by a previous insertion, and
			// somewhere in the (implicit) compressed path we took a
			// wrong turn, into the wrong part of st's subtree.
			//
			// This is okay, because pfx and child.prefix must have a
			// common ancestor node somewhere between st and child. We
			// can figure out what node that is, and materialize it.
			//
			// Once we've done that, we can immediately complete the
			// remainder of the insertion in one of two ways, without
			// further traversal. See a little further down for what
			// those are.
			if debugInsert {
				fmt.Printf("insert: wrong turn, pfx=%s child.prefix=%s\n", pfx, child.prefix)
			}
			intermediatePrefix, addrOfExisting, addrOfNew := computePrefixSplit(child.prefix, pfx)
			intermediate := &strideTable[T]{prefix: intermediatePrefix} // TODO: make this whole thing be st.AddIntermediate or something?
			st.setChild(bs[byteIdx], intermediate)
			intermediate.setChild(addrOfExisting, child)

			if debugInsert {
				fmt.Printf("insert: new intermediate st.prefix=%s intermediate.prefix=%s child.prefix=%s\n", st.prefix, intermediate.prefix, child.prefix)
			}

			// Now, we have a chain of st -> intermediate -> child.
			//
			// pfx either lives in a different child of intermediate,
			// or in intermediate itself. For example, if we created
			// the intermediate 1.2.0.0/16, pfx=1.2.3.4/32 would have
			// to go into a new child of intermediate, but
			// pfx=1.2.0.0/18 would go into intermediate directly.
			if remain := pfx.Bits() - intermediate.prefix.Bits(); remain <= 8 {
				// pfx lives in intermediate.
				if debugInsert {
					fmt.Printf("insert: into intermediate intermediate.prefix=%s addr=%d/%d\n", intermediate.prefix, bs[finalByteIdx], finalBits)
				}
				intermediate.insert(bs[finalByteIdx], finalBits, val)
			} else {
				// pfx lives in a different child subtree of
				// intermediate. By definition this subtree doesn't
				// exist at all, otherwise we'd never have entered
				// this entire "wrong turn" codepath in the first
				// place.
				//
				// This means we can apply prefix compression as we
				// create this new child, and we're done.
				st, created = intermediate.getOrCreateChild(addrOfNew)
				if !created {
					panic("new child path unexpectedly exists during path decompression")
				}
				st.prefix = finalStridePrefix
				st.insert(bs[finalByteIdx], finalBits, val)
				if debugInsert {
					fmt.Printf("insert: new child st.prefix=%s addr=%d/%d\n", st.prefix, bs[finalByteIdx], finalBits)
				}
			}

			return
		default:
			// An expected child table exists along pfx's
			// path. Continue traversing downwards.
			st = child
			byteIdx = child.prefix.Bits() / 8
			numBits = pfx.Bits() - child.prefix.Bits()
			if debugInsert {
				fmt.Printf("insert: descend st.prefix=%s\n", st.prefix)
			}
		}
	}
}

// Delete removes pfx from the table, if it is present.
func (t *Table[T]) Delete(pfx netip.Prefix) {
	t.init()

	// The standard library doesn't enforce normalized prefixes (where
	// the non-prefix bits are all zero). These algorithms require
	// normalized prefixes, so do it upfront.
	pfx = pfx.Masked()

	if debugDelete {
		defer func() {
			fmt.Printf("%s", t.debugSummary())
		}()
		fmt.Printf("\ndelete: start pfx=%s table:\n%s", pfx, t.debugSummary())
	}

	st := t.tableForAddr(pfx.Addr())

	// This algorithm is full of off-by-one headaches, just like
	// Insert. See the comment in Insert for more details. Bottom
	// line: we handle the default route as a special case, and that
	// simplifies the rest of the code slightly.
	if pfx.Bits() == 0 {
		if debugDelete {
			fmt.Printf("delete: default route\n")
		}
		st.delete(0, 0)
		return
	}

	// Deletion may drive the refcount of some strideTables down to
	// zero. We need to clean up these dangling tables, so we have to
	// keep track of which tables we touch on the way down, and which
	// strideEntry index each child is registered in.
	//
	// Note that the strideIndex and strideTables entries are off-by-one.
	// The child table pointer is recorded at i+1, but it is referenced by a
	// particular index in the parent table, at index i.
	//
	// In other words: entry number strideIndexes[0] in
	// strideTables[0] is the same pointer as strideTables[1].
	//
	// This results in some slightly odd array accesses further down
	// in this code, because in a single loop iteration we have to
	// write to strideTables[N] and strideIndexes[N-1].
	strideIdx := 0
	strideTables := [16]*strideTable[T]{st}
	strideIndexes := [15]uint8{}

	// Similar to Insert, navigate down the tree of strideTables,
	// looking for the one that houses this prefix. This part is
	// easier than with insertion, since we can bail if the path ends
	// early or takes an unexpected detour.  However, unlike
	// insertion, there's a whole post-deletion cleanup phase later
	// on.
	//
	// As we walk down the tree, byteIdx is the byte of bs we're
	// currently examining to choose our next step, and numBits is the
	// number of bits that remain in pfx, starting with the byte at
	// byteIdx inclusive.
	bs := pfx.Addr().AsSlice()
	byteIdx := 0
	numBits := pfx.Bits()
	for numBits > 8 {
		if debugDelete {
			fmt.Printf("delete: loop byteIdx=%d numBits=%d st.prefix=%s\n", byteIdx, numBits, st.prefix)
		}
		child := st.getChild(bs[byteIdx])
		if child == nil {
			// Prefix can't exist in the table, because one of the
			// necessary strideTables doesn't exist.
			if debugDelete {
				fmt.Printf("delete: missing necessary child pfx=%s\n", pfx)
			}
			return
		}
		strideIndexes[strideIdx] = bs[byteIdx]
		strideTables[strideIdx+1] = child
		strideIdx++

		// Path compression means byteIdx can jump forwards
		// unpredictably. Recompute the next byte to look at from the
		// child we just found.
		byteIdx = child.prefix.Bits() / 8
		numBits = pfx.Bits() - child.prefix.Bits()
		st = child

		if debugDelete {
			fmt.Printf("delete: descend st.prefix=%s\n", st.prefix)
		}
	}

	// We reached a leaf stride table that seems to be in the right
	// spot. But path compression might have led us to the wrong
	// table.
	if !prefixStrictlyContains(st.prefix, pfx) {
		// Wrong table, the requested prefix can't exist since its
		// path led us to the wrong place.
		if debugDelete {
			fmt.Printf("delete: wrong leaf table pfx=%s\n", pfx)
		}
		return
	}
	if debugDelete {
		fmt.Printf("delete: delete from st.prefix=%s addr=%d/%d\n", st.prefix, bs[byteIdx], numBits)
	}
	if routeExisted := st.delete(bs[byteIdx], numBits); !routeExisted {
		// We're in the right strideTable, but pfx wasn't in
		// it. Refcounts haven't changed, so we can skip cleanup.
		if debugDelete {
			fmt.Printf("delete: prefix not present pfx=%s\n", pfx)
		}
		return
	}

	// st.delete reduced st's refcount by one. This table may now be
	// reclaimable, and depending on how we can reclaim it, the parent
	// tables may also need to be reclaimed. This loop ends as soon as
	// an iteration takes no action, or takes an action that doesn't
	// alter the parent table's refcounts.
	//
	// We start our walk back at strideTables[strideIdx], which
	// contains st.
	for strideIdx > 0 {
		cur := strideTables[strideIdx]
		if debugDelete {
			fmt.Printf("delete: GC? strideIdx=%d st.prefix=%s\n", strideIdx, cur.prefix)
		}
		if cur.routeRefs > 0 {
			// the strideTable has other route entries, it cannot be
			// deleted or compacted.
			if debugDelete {
				fmt.Printf("delete: has other routes st.prefix=%s\n", cur.prefix)
			}
			return
		}
		switch cur.childRefs {
		case 0:
			// no routeRefs and no childRefs, this table can be
			// deleted. This will alter the parent table's refcount,
			// so we'll have to look at it as well (in the next loop
			// iteration).
			if debugDelete {
				fmt.Printf("delete: remove st.prefix=%s\n", cur.prefix)
			}
			strideTables[strideIdx-1].deleteChild(strideIndexes[strideIdx-1])
			strideIdx--
		case 1:
			// This table has no routes, and a single child. Compact
			// this table out of existence by making the parent point
			// directly at the one child. This does not affect the
			// parent's refcounts, so the parent can't be eligible for
			// deletion or compaction, and we can stop.
			child := strideTables[strideIdx].findFirstChild() // only 1 child exists, by definition
			parent := strideTables[strideIdx-1]
			if debugDelete {
				fmt.Printf("delete: compact parent.prefix=%s st.prefix=%s child.prefix=%s\n", parent.prefix, cur.prefix, child.prefix)
			}
			strideTables[strideIdx-1].setChild(strideIndexes[strideIdx-1], child)
			return
		default:
			// This table has two or more children, so it's acting as a "fork in
			// the road" between two prefix subtrees. It cannot be deleted, and
			// thus no further cleanups are possible.
			if debugDelete {
				fmt.Printf("delete: fork table st.prefix=%s\n", cur.prefix)
			}
			return
		}
	}
}

// OverlapsPrefix reports whether any IP in pfx matches a route in the
// table.
func (t *Table[T]) OverlapsPrefix(pfx netip.Prefix) bool {
	// Single IPs can be checked faster with Get.
	if pfx.IsSingleIP() {
		_, ok := t.Get(pfx.Addr())
		return ok
	}

	t.init()

	pfx = pfx.Masked()
	st := t.tableForAddr(pfx.Addr())

	// Similar to other methods, handle the default route specially so
	// that the rest of the algorithm doesn't have to wrestle with
	// off-by-one error. Plus, the overlap check for the default route
	// is trivial.
	if pfx.Bits() == 0 && (st.routeRefs > 0 || st.childRefs > 0) {
		return true
	}

	rawAddr := pfx.Addr().As16()
	bs := rawAddr[:]
	if pfx.Addr().Is4() {
		bs = bs[12:]
	}

	byteIdx := 0
	numBits := pfx.Bits()
	for {
		if !prefixContains(st.prefix, pfx) {
			// Path compression has led us off the strideTable path
			// that would contain some overlapping prefix. There is no
			// overlap, we're done.
			//
			// We could choose to do the same optimization as in Get
			// here, and not check the strideTable prefixes until
			// we've finished descending through all the
			// tables. However, when looking for prefix overlaps, it's
			// more likely that we'll find a short-cutting answer
			// early in the tree descent, and doing the check during
			// descent simplifies the code. Contrast this to Get,
			// where we're searching for a maximally long prefix
			// (which is what individual addresses are), so the answer
			// we're looking for is almost certainly at the bottom of
			// the tree, with a few exceptions caused by path
			// compression.
			return false
		}
		if numBits <= 8 {
			// Reached the end of the prefix, do the final overlap
			// check on the remaining bits.
			finalByteIdx := (pfx.Bits() - 1) / 8
			finalBits := pfx.Bits() - (finalByteIdx * 8)
			return st.overlapsPrefix(bs[finalByteIdx], finalBits)
		}

		// Still in the middle of the prefix. Any match at this stage
		// means that some route exists which overlaps pfx, so we can
		// cut short. Otherwise, we have to keep descending through
		// child tables.
		_, hasRoute, child := st.getValAndChild(bs[byteIdx])
		if hasRoute {
			return true
		}

		if child == nil {
			// Ran out of children to explore, there can't be an
			// overlap.
			return false
		}

		st = child
		byteIdx = st.prefix.Bits() / 8
		numBits = pfx.Bits() - child.prefix.Bits()
	}
}

func (t *Table[T]) Overlaps(o *Table[T]) bool {
	if debugOverlaps {
		fmt.Printf("Overlaps start\n")
	}
	return overlapsSameStride(&t.v4, &o.v4) || overlapsSameStride(&t.v6, &o.v6)
}

func overlapsSameStride[T any](t, o *strideTable[T]) bool {
	if debugOverlaps {
		fmt.Printf("overlapsSameStride %s %s\n", t.prefix, o.prefix)
		defer fmt.Printf("exit: overlapsSameStride %s %s\n", t.prefix, o.prefix)
	}

	if t.entriesOverlap(o) {
		return true
	}

	for i := 0; i < 256; i++ {
		_, troute, tchild := t.getValAndChild(uint8(i))
		_, oroute, ochild := o.getValAndChild(uint8(i))

		// Compare each host slot in t and o. Slots can overlap in a
		// variety of ways as the route entries and child tables
		// combine:
		switch {
		case troute && oroute:
			// Easy overlap of route entries.
			if debugOverlaps {
				fmt.Printf("%d: stride entries overlap\n", i)
			}
			return true
		case troute && ochild != nil:
			// no immediate route in o, but o has child prefixes at a
			// place where t has a route. They must overlap somewhere
			// within ochild's subtree.
			if debugOverlaps {
				fmt.Printf("%d: left route overlaps with right child\n", i)
			}
			return true
		case oroute && tchild != nil:
			// Same but in reverse, with tchild having the overlapping
			// subtree.
			if debugOverlaps {
				fmt.Printf("%d: left child overlaps with right route\n", i)
			}
			return true
		case tchild == nil && ochild == nil:
			// No children to explore here, we can just keep iterating.
			continue
		case tchild == nil || ochild == nil:
			// one child but no counterpart in the other tree, no overlap
			// can come from here.
			continue
		case tchild.prefix == ochild.prefix:
			// The trees at this location are identical in t and
			// o. Descend and check, exiting early if this subtree
			// shows an overlap.
			if debugOverlaps {
				fmt.Printf("%d: children are same stride\n", i)
			}
			if overlapsSameStride(tchild, ochild) {
				return true
			}
			continue
		case prefixStrictlyContains(tchild.prefix, ochild.prefix):
			// Path compression has made the subtrees diverge, but tchild
			// is a parent of ochild. Descend and check, using the
			// parent/child aware logic.
			if debugOverlaps {
				fmt.Printf("%d: left child is parent of right child\n", i)
			}
			if overlapsParentChild(tchild, ochild) {
				return true
			}
			continue
		case prefixStrictlyContains(ochild.prefix, tchild.prefix):
			// Same, but in reverse, ochild is the parent.
			if debugOverlaps {
				fmt.Printf("%d: right child is parent of left child\n", i)
			}
			if overlapsParentChild(ochild, tchild) {
				return true
			}
			continue
		default:
			// tchild and ochild both exist, but refer to disjoin subtrees
			// due to path compression. This subtree does not represent an
			// overlap.
			continue
		}
	}
	return false
}

func overlapsParentChild[T any](t, o *strideTable[T]) bool {
	if debugOverlaps {
		fmt.Printf("overlapsParentChild parent=%s child=%s\n", t.prefix, o.prefix)
		defer fmt.Printf("exit: overlapsParentChild parent=%s child=%s\n", t.prefix, o.prefix)
	}
	// t is definitely a parent of o, so we have at least 8 bits of
	// prefix between t and o that we can use to find the correct
	// child entry and pointer.
	nextByte := t.prefix.Bits() / 8
	var addr uint8
	if o.prefix.Addr().Is4() {
		addr = o.prefix.Addr().As4()[nextByte]
	} else {
		addr = o.prefix.Addr().As16()[nextByte]
	}
	if debugOverlaps {
		fmt.Printf("child byte lookup: %d\n", addr)
	}

	_, hasRoute, tc := t.getValAndChild(addr)

	switch {
	case hasRoute:
		// t has a route covering o, they must overlap.
		if debugOverlaps {
			fmt.Printf("parent has route that covers child\n")
		}
		return true
	case tc == nil:
		// t has no route itself (previous case), and no child
		// corresponding to o. No overlap possible.
		if debugOverlaps {
			fmt.Printf("parent has no route or relevant child\n")
		}
		return false
	case tc.prefix == o.prefix:
		// The two prefix trees have resynchronized, can go back to
		// exploring them with the main comparison logic.
		if debugOverlaps {
			fmt.Printf("parent and child resynced at %s, continue\n", tc.prefix)
		}
		return overlapsSameStride(tc, o)
	case prefixStrictlyContains(tc.prefix, o.prefix):
		// t's child is still a parent of o. Keep descending with this
		// logic.
		if debugOverlaps {
			fmt.Printf("parent.child=%s still above child=%s, descend\n", tc.prefix, o.prefix)
		}
		return overlapsParentChild(tc, o)
	case prefixStrictlyContains(o.prefix, tc.prefix):
		// t's child jumped over o, o is now the parent. Still use
		// this logic, but swap the parent/child relationship.
		if debugOverlaps {
			fmt.Printf("child=%s now above parent.child=%s, swap and descend\n", o.prefix, tc.prefix)
		}
		return overlapsParentChild(o, tc)
	default:
		// t's child and o represent disjoint subtrees, no overlap
		// possible.
		if debugOverlaps {
			fmt.Printf("parent.child=%s has diverged from child=%s, no overlap\n", tc.prefix, o.prefix)
		}
		return false
	}
}

// debugSummary prints the tree of allocated strideTables in t, with each
// strideTable's refcount.
func (t *Table[T]) debugSummary() string {
	t.init()
	var ret bytes.Buffer
	fmt.Fprintf(&ret, "v4: ")
	strideSummary(&ret, &t.v4, 4)
	fmt.Fprintf(&ret, "v6: ")
	strideSummary(&ret, &t.v6, 4)
	return ret.String()
}

func strideSummary[T any](w io.Writer, st *strideTable[T], indent int) {
	fmt.Fprintf(w, "%s: %d routes, %d children\n", st.prefix, st.routeRefs, st.childRefs)
	indent += 4
	st.treeDebugStringRec(w, 1, indent)
	for addr, child := range st.children {
		if child == nil {
			continue
		}
		fmt.Fprintf(w, "%s%d/8 (%02x/8): ", strings.Repeat(" ", indent), addr, addr)
		strideSummary(w, child, indent)
	}
}

// prefixContains reports whether child is a prefix within parent, or
// is equal to parent.
func prefixContains(parent, child netip.Prefix) bool {
	return parent.Overlaps(child) && parent.Bits() <= child.Bits()
}

// prefixStrictlyContains reports whether child is a prefix within
// parent, but not parent itself.
func prefixStrictlyContains(parent, child netip.Prefix) bool {
	return parent.Overlaps(child) && parent.Bits() < child.Bits()
}

// computePrefixSplit returns the smallest common prefix that contains
// both a and b. lastCommon is 8-bit aligned, with aStride and bStride
// indicating the value of the 8-bit stride immediately following
// lastCommon.
//
// computePrefixSplit is used in constructing an intermediate
// strideTable when a new prefix needs to be inserted in a compressed
// table. It can be read as: given that a is already in the table, and
// b is being inserted, what is the prefix of the new intermediate
// strideTable that needs to be created, and at what addresses in that
// new strideTable should a and b's subsequent strideTables be
// attached?
//
// Note as a special case, this can be called with a==b. An example of
// when this happens:
//   - We want to insert the prefix 1.2.0.0/16
//   - A strideTable exists for 1.2.0.0/16, because another child
//     prefix already exists (e.g. 1.2.3.4/32)
//   - The 1.0.0.0/8 strideTable does not exist, because path
//     compression removed it.
//
// In this scenario, the caller of computePrefixSplit ends up making a
// "wrong turn" while traversing strideTables: it was looking for the
// 1.0.0.0/8 table, but ended up at the 1.2.0.0/16 table. When this
// happens, it will invoke computePrefixSplit(1.2.0.0/16, 1.2.0.0/16),
// and we return 1.0.0.0/8 as the missing intermediate.
func computePrefixSplit(a, b netip.Prefix) (lastCommon netip.Prefix, aStride, bStride uint8) {
	a = a.Masked()
	b = b.Masked()
	if a.Bits() == 0 || b.Bits() == 0 {
		panic("computePrefixSplit called with a default route")
	}
	if a.Addr().Is4() != b.Addr().Is4() {
		panic("computePrefixSplit called with mismatched address families")
	}

	minPrefixLen := a.Bits()
	if b.Bits() < minPrefixLen {
		minPrefixLen = b.Bits()
	}

	commonBits := commonBits(a.Addr(), b.Addr(), minPrefixLen)
	// We want to know how many 8-bit strides are shared between a and
	// b. Naively, this would be commonBits/8, but this introduces an
	// off-by-one error. This is due to the way our ART stores
	// prefixes whose length falls exactly on a stride boundary.
	//
	// Consider 192.168.1.0/24 and 192.168.0.0/16. commonBits
	// correctly reports that these prefixes have their first 16 bits
	// in common. However, in the ART they only share 1 common stride:
	// they both use the 192.0.0.0/8 strideTable, but 192.168.0.0/16
	// is stored as 168/8 within that table, and not as 0/0 in the
	// 192.168.0.0/16 table.
	//
	// So, when commonBits matches the length of one of the inputs and
	// falls on a boundary between strides, the strideTable one
	// further up from commonBits/8 is the one we need to create,
	// which means we have to adjust the stride count down by one.
	if commonBits == minPrefixLen {
		commonBits--
	}
	commonStrides := commonBits / 8
	lastCommon, err := a.Addr().Prefix(commonStrides * 8)
	if err != nil {
		panic(fmt.Sprintf("computePrefixSplit constructing common prefix: %v", err))
	}
	if a.Addr().Is4() {
		aStride = a.Addr().As4()[commonStrides]
		bStride = b.Addr().As4()[commonStrides]
	} else {
		aStride = a.Addr().As16()[commonStrides]
		bStride = b.Addr().As16()[commonStrides]
	}
	return lastCommon, aStride, bStride
}

// commonBits returns the number of common leading bits of a and b.
// If the number of common bits exceeds maxBits, it returns maxBits
// instead.
func commonBits(a, b netip.Addr, maxBits int) int {
	if a.Is4() != b.Is4() {
		panic("commonStrides called with mismatched address families")
	}
	var common int
	// The following implements an old bit-twiddling trick to compute
	// the number of common leading bits: if you XOR two numbers
	// together, equal bits become 0 and unequal bits become 1. You
	// can then count the number of leading zeros (which is a single
	// instruction on modern CPUs) to get the answer.
	//
	// This code is a little more complex than just XOR + count
	// leading zeros, because IPv4 and IPv6 are different sizes, and
	// for IPv6 we have to do the math in two 64-bit chunks because Go
	// lacks a uint128 type.
	if a.Is4() {
		aNum, bNum := ipv4AsUint(a), ipv4AsUint(b)
		common = bits.LeadingZeros32(aNum ^ bNum)
	} else {
		aNumHi, aNumLo := ipv6AsUint(a)
		bNumHi, bNumLo := ipv6AsUint(b)
		common = bits.LeadingZeros64(aNumHi ^ bNumHi)
		if common == 64 {
			common += bits.LeadingZeros64(aNumLo ^ bNumLo)
		}
	}
	if common > maxBits {
		common = maxBits
	}
	return common
}

// ipv4AsUint returns ip as a uint32.
func ipv4AsUint(ip netip.Addr) uint32 {
	bs := ip.As4()
	return binary.BigEndian.Uint32(bs[:])
}

// ipv6AsUint returns ip as a pair of uint64s.
func ipv6AsUint(ip netip.Addr) (uint64, uint64) {
	bs := ip.As16()
	return binary.BigEndian.Uint64(bs[:8]), binary.BigEndian.Uint64(bs[8:])
}
