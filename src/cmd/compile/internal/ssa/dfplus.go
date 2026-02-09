// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"slices"
)

const debugDFPlus = false

// dfPlus calculates the iterated dominance frontier of a function.
func dfPlus(f *Func) [][]*Block {
	// Our SSA construction code doesn't expose the dominance frontier to the backend passes
	// and it only constructs DF+ sets for individual variables. Even if it did expose this information,
	// the CFG may have changed and invalidated the result. We have to calculate it here ourselves.
	//
	// We use the TDMSC-I algorithm from Das and Ramakrishna here: https://dl.acm.org/doi/epdf/10.1145/1065887.1065890
	// It is a linear time algorithm for calculating the DF+ set of every single
	// block. This means that we can construct arbitrary DF+ sets over variables as we insert them
	// into the function.
	//
	// The original TDMSC-I algorithm uses an explicit set to keep track of which edges have already been
	// visited during a given iteration. Here, we instead order our processing so
	// that we can determine whether we've visited a node by checking our current position
	// in the processing
	//
	// TODO: We can probably use the DF+ for something elsewhere. Consider caching it
	// like we do the dominator information.

	dfplus := make([][]*Block, f.NumBlocks())
	sdom := f.Sdom()
	// TODO(dmo): Does a numbering in sparsetree allow
	// for looking up the bID to BFS order already?
	// We have to keep a queue anyway, so it's not a big loss
	bidToBFS := f.Cache.allocIntSlice(f.NumBlocks())
	defer f.Cache.freeIntSlice(bidToBFS)
	bfsOrder := f.Cache.allocBlockSlice(f.NumBlocks())
	defer f.Cache.freeBlockSlice(bfsOrder)

	bfsOrder = bfsOrder[:0]
	bfsOrder = append(bfsOrder, f.Entry)
	parent := f.Entry
	bidToBFS[parent.ID] = 0

	i := 0
	for i < len(bfsOrder) {
		parent := bfsOrder[i]
		// TODO(dmo): the order we visit the dom tree impacts whether we need
		// to do consistency checking. Right now, we use the post-order
		// that sparseTree gives us, but is there an alternate order that minimizes
		// the number of consistency checks or iterations until stabilization?
		for child := sdom.Child(parent); child != nil; child = sdom.Sibling(child) {
			bidToBFS[child.ID] = len(bfsOrder)
			bfsOrder = append(bfsOrder, child)
		}
		i++
	}
	if debugDFPlus {
		fmt.Println(f.Name)
		fmt.Println(bfsOrder)
		fmt.Println(sdom.treestructure(f.Entry))
	}
	dfpSet := f.Cache.allocSparseSet(f.NumBlocks())
	defer f.Cache.freeSparseSet(dfpSet)
	pass := 1
	for {
		if debugDFPlus {
			fmt.Printf("%s pass %d\n", f.Name, pass)
		}
		pass++
		consistent := true
		for bfsIdx, b := range bfsOrder {
			for outerPredIdx, e := range b.Preds {
				pred := e.b
				// only consider incoming J edges
				if sdom.isAncestor(pred, b) {
					continue
				}
				if debugDFPlus {
					fmt.Printf("[edge %v(%v)->%v(%v)]\n", pred, bidToBFS[pred.ID], b, bfsIdx)
				}
				// Walk up the dominator tree of the incoming
				// edge's source, adding DF+(b) ∪ b to their
				// DF+ set.
				var lastParent *Block
				parent := pred
				for sdom.NumAncestors(parent) >= sdom.NumAncestors(b) {
					if debugDFPlus {
						fmt.Printf("\tadding DF+(%v) (l:%v) %v+[%v] to DF+(%v) (l:%v) %v\n", b, sdom.NumAncestors(b), dfplus[b.ID], b, parent, sdom.NumAncestors(parent), dfplus[parent.ID])
					}
					dfplus[parent.ID] = updateDFPlus(dfpSet, dfplus[parent.ID], b, dfplus[b.ID])
					lastParent = parent
					parent = sdom.Parent(parent)
				}
				// consistency checking is fairly cheap but not entirely free.
				// Skip it if we've already found an inconsistency
				if consistent {
					if debugDFPlus {
						fmt.Printf("lastParent %v\n", lastParent)
					}
				check:
					for predIdx, e := range lastParent.Preds {
						pred := e.b
						// again, only consider incoming J edges.
						if sdom.IsAncestorEq(pred, lastParent) {
							continue
						}
						if debugDFPlus {
							fmt.Printf("\t[%v(%v)->%v(%v)] ", pred, bidToBFS[pred.ID], lastParent, bidToBFS[lastParent.ID])
						}
						if bidToBFS[lastParent.ID] > bfsIdx ||
							(b.ID == lastParent.ID && outerPredIdx < predIdx) {
							if debugDFPlus {
								fmt.Printf("not visited\n")
							}
							continue
						}
						if debugDFPlus {
							fmt.Printf("checking df+(pred) = %v, df+(lastParent) = %v\n", dfplus[pred.ID], dfplus[lastParent.ID])
						}
						dfpSet.clear()
						for _, d := range dfplus[pred.ID] {
							dfpSet.add(d.ID)
						}
						for _, d := range dfplus[lastParent.ID] {
							if !dfpSet.contains(d.ID) {
								if debugDFPlus {
									fmt.Printf("found inconsistent\n")
								}
								consistent = false
								// No need to check the other predecessors
								break check
							}
						}
					}
				}
			}
		}
		if consistent {
			break
		}
	}
	return dfplus
}

func updateDFPlus(dfpSet *sparseSet, old []*Block, b *Block, add []*Block) []*Block {
	// allocating in a loop is a significant part of the cost
	// of running the algorithm. Figure out how big the array needs to be
	// before we allocate a right sized slice.
	dfpSet.clear()
	for _, d := range old {
		dfpSet.add(d.ID)
	}
	nAdd := 0
	if !dfpSet.contains(b.ID) {
		nAdd += 1
	}
	for _, d := range add {
		if !dfpSet.contains(d.ID) {
			nAdd += 1
		}
	}
	if nAdd == 0 {
		return old
	}
	new := slices.Grow(old, nAdd)
	if !dfpSet.contains(b.ID) {
		new = append(new, b)
		dfpSet.add(b.ID)
	}
	for _, d := range add {
		if !dfpSet.contains(d.ID) {
			new = append(new, d)
		}
	}
	return new
}
