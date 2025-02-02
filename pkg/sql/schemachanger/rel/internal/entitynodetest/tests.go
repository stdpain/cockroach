// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package entitynodetest

import (
	"reflect"

	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel/reltest"
)

type v = rel.Var

var (
	// Suite defines the entitynode test suite.
	Suite = reltest.Suite{
		Name:           "entitynode",
		Schema:         schema,
		Registry:       r,
		DatabaseTests:  databaseTests,
		AttributeTests: attributeCases,
	}

	r  = reltest.NewRegistry()
	a  = r.FromYAML("a", `{i16: 1, i8: 1, pi8: 1}`, &entity{}).(*entity)
	b  = r.FromYAML("b", `{i16: 2, i8: 2}`, &entity{}).(*entity)
	c  = r.FromYAML("c", `{i16: 1, i8: 2}`, &entity{}).(*entity)
	na = r.Register("na", &node{Value: a}).(*node)
	nb = r.Register("nb", &node{Value: b, Left: na}).(*node)
	nc = r.Register("nc", &node{Value: c, Right: nb}).(*node)

	nodeEntity = schema.Def2("nodeEntity", "node", "entity", func(
		node, entity rel.Var,
	) rel.Clauses {
		return rel.Clauses{
			node.AttrEqVar(value, entity),
		}
	})

	// passThrough is used to check rewriting of variables in rule invocations
	// works.
	passThrough = schema.Def4("passThrough", "a", "b", "c", "d", func(
		a, b, c, d rel.Var,
	) rel.Clauses {
		return rel.Clauses{
			rightLeft(a, b, c, d),
		}
	})
	rightLeft = schema.Def4("rightLeft", "root", "right", "right-left", "v", func(
		root, rightN, rightLeft, rightLeftV rel.Var,
	) rel.Clauses {
		return rel.Clauses{
			rel.And(
				root.Type((*node)(nil)),
				root.AttrEqVar(right, rightN),
			),
			rightN.AttrEqVar(left, rightLeft),
			rightN.Type((*node)(nil)),
			rightLeft.AttrEqVar(value, rightLeftV),
			rightLeftV.Eq(a),
			rel.Filter("noop", rightLeft)(func(n *node) bool {
				return true
			}),
		}
	})

	databaseTests = []reltest.DatabaseTest{
		{
			Data: []string{"a", "b", "c", "na", "nb", "nc"},
			Indexes: [][]rel.Index{
				{{}}, // 0
				{ // 1
					{Attrs: []rel.Attr{value}},
					{Attrs: []rel.Attr{pi8}},
					{Attrs: []rel.Attr{i8, i16}},
				},
				{ // 2
					{
						Attrs: []rel.Attr{value},
						Where: []rel.IndexWhere{
							{Attr: rel.Type, Eq: reflect.TypeOf((*node)(nil))},
						},
					},
					{
						Attrs: []rel.Attr{pi8},
						Where: []rel.IndexWhere{
							{Attr: rel.Type, Eq: reflect.TypeOf((*entity)(nil))},
						},
					},
					{
						Attrs: []rel.Attr{i8, i16},
						Where: []rel.IndexWhere{
							{Attr: rel.Type, Eq: reflect.TypeOf((*entity)(nil))},
						},
					},
				},
				{ // 3
					{
						Attrs: []rel.Attr{value},
						Where: []rel.IndexWhere{
							{Attr: rel.Type, Eq: reflect.TypeOf((*node)(nil))},
						},
					},
					{
						Attrs:  []rel.Attr{pi8},
						Exists: []rel.Attr{pi8},
					},
					{
						Attrs: []rel.Attr{i8, i16},
						Where: []rel.IndexWhere{
							{Attr: rel.Type, Eq: reflect.TypeOf((*entity)(nil))},
						},
					},
				},
				{ // 4
					{Attrs: []rel.Attr{rel.Type}},
					{Attrs: []rel.Attr{rel.Self}},
				},
				{ // 5
					{Attrs: []rel.Attr{rel.Self}},
				},
				{ // 6
					{Attrs: []rel.Attr{rel.Self}, Exists: []rel.Attr{rel.Self}},
				},
			},
			QueryCases: []reltest.QueryTest{
				{
					Name: "a fields",
					Query: rel.Clauses{
						v("a").Type((*entity)(nil)),
						v("a").AttrEq(i16, int16(1)),
						v("a").AttrEqVar(i8, "ai8"),
						v("a").AttrEqVar(pi8, "api8"),
					},
					Entities: []rel.Var{"a"},
					ResVars:  []v{"a", "ai8", "api8"},
					Results: [][]interface{}{
						{a, int8(1), int8(1)},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "a-c-b join",
					Query: rel.Clauses{
						v("a").AttrEq(i8, int8(1)),
						v("b").AttrEq(i16, int16(2)),
						v("b").AttrEq(i8, int8(2)),
						v("c").AttrEq(i16, int16(1)),
						v("c").AttrEq(i8, int8(2)),
					},
					Entities: []v{"a", "b", "c"},
					ResVars:  []v{"a", "b", "c"},
					Results: [][]interface{}{
						{a, b, c},
					},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "nil values don't show up",
					Query: rel.Clauses{
						v("value").AttrEq(pi8, int8(1)),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value"},
					Results: [][]interface{}{
						{a},
					},
					UnsatisfiableIndexes: []int{2, 4, 5, 6},
				},
				{
					Name: "nil values don't show up, scalar pointers same as pointers",
					Query: rel.Clauses{
						v("value").AttrEq(pi8, newInt8(1)),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value"},
					Results: [][]interface{}{
						{a},
					},
					UnsatisfiableIndexes: []int{2, 4, 5, 6},
				},
				{
					Name: "list all the values",
					Query: rel.Clauses{
						v("value").AttrEqVar(i8, "i8"),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value", "i8"},
					Results: [][]interface{}{
						{a, int8(1)},
						{b, int8(2)},
						{c, int8(2)},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "list all the values with type constraint",
					Query: rel.Clauses{
						v("value").AttrEqVar(i8, "i8"),
						v("value").Type((*entity)(nil)),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value", "i8"},
					Results: [][]interface{}{
						{a, int8(1)},
						{b, int8(2)},
						{c, int8(2)},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "nodes with elements where i8=2",
					Query: rel.Clauses{
						v("i8").Eq(int8(2)),
						v("i8").Entities(i8, "value"), // using this notation just to exercise it
						v("n").AttrEqVar(value, "value"),
					},
					Entities: []v{"value", "n"},
					ResVars:  []v{"n", "value"},
					Results: [][]interface{}{
						{nb, b},
						{nc, c},
					},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "nodes with elements where i8=2 (rule)",
					Query: rel.Clauses{
						v("i8").Eq(int8(2)),
						v("i8").Entities(i8, "value"), // using this notation just to exercise it
						nodeEntity("n", "value"),
					},
					Entities: []v{"value", "n"},
					ResVars:  []v{"n", "value"},
					Results: [][]interface{}{
						{nb, b},
						{nc, c},
					},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "list all the i8 values",
					Query: rel.Clauses{
						v("value").AttrEqVar(i8, "i8"),
						v("value").Type((*entity)(nil)),
					},
					Entities: []v{"value"},
					ResVars:  []v{"i8"},
					// Note that you get the value for all the entities
					// which can offer it. That's maybe surprising.
					Results: [][]interface{}{
						{int8(1)},
						{int8(2)},
						{int8(2)},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "use a filter",
					Query: rel.Clauses{
						v("value").AttrEqVar(rel.Self, "_"),
						rel.Filter("i8eq1", "value")(func(entity *entity) bool {
							return entity.I8 == 1
						}),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value"},
					Results: [][]interface{}{
						{a},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "types of all the entities",
					Query: rel.Clauses{
						v("value").AttrEqVar(rel.Type, "typ"),
					},
					Entities: []v{"value"},
					ResVars:  []v{"value", "typ"},
					Results: [][]interface{}{
						{a, reflect.TypeOf((*entity)(nil))},
						{b, reflect.TypeOf((*entity)(nil))},
						{c, reflect.TypeOf((*entity)(nil))},
						{na, reflect.TypeOf((*node)(nil))},
						{nb, reflect.TypeOf((*node)(nil))},
						{nc, reflect.TypeOf((*node)(nil))},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "nodes by type",
					Query: rel.Clauses{
						v("na").Type((*node)(nil)),
						v("na").AttrEqVar(value, "a"),
						v("nb").AttrEqVar(left, "na"),
						v("nc").AttrEqVar(right, "nb"),
					},
					Entities: []v{"na", "nb", "nc"},
					ResVars:  []v{"na", "nb", "nc", "a"},
					Results: [][]interface{}{
						{na, nb, nc, a},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "nodes with rule",
					Query: rel.Clauses{
						passThrough("nc", "nb", "na", "a"),
					},
					Entities: []v{"nc", "nb", "na"},
					ResVars:  []v{"nc", "nb", "na", "a"},
					Results: [][]interface{}{
						{nc, nb, na, a},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "list nodes",
					Query: rel.Clauses{
						v("n").Type((*node)(nil)),
					},
					Entities: []v{"n"},
					ResVars:  []v{"n"},
					Results: [][]interface{}{
						{na},
						{nb},
						{nc},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "basic any",
					Query: rel.Clauses{
						v("entity").Type((*node)(nil), (*entity)(nil)),
					},
					Entities: []v{"entity"},
					ResVars:  []v{"entity"},
					Results: [][]interface{}{
						{a},
						{b},
						{c},
						{na},
						{nb},
						{nc},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 5, 6},
				},
				{
					Name: "self eq value",
					Query: rel.Clauses{
						v("entity").AttrEq(rel.Self, c),
					},
					Entities: []v{"entity"},
					ResVars:  []v{"entity"},
					Results: [][]interface{}{
						{c},
					},
					UnsatisfiableIndexes: []int{1, 2, 3},
				},
				{
					Name: "contradiction due to missing attribute",
					Query: rel.Clauses{
						v("entity").AttrEq(rel.Self, c),
						v("entity").AttrEqVar(pi8, "pi8"),
					},
					Entities:             []v{"entity"},
					ResVars:              []v{"entity", "pi8"},
					Results:              [][]interface{}{},
					UnsatisfiableIndexes: []int{1, 2, 3},
				},
				{
					Name: "self eq self",
					Query: rel.Clauses{
						v("entity").AttrEqVar(rel.Self, "entity"), // all entities
					},
					Entities: []v{"entity"},
					ResVars:  []v{"entity"},
					Results: [][]interface{}{
						{a}, {b}, {c}, {na}, {nb}, {nc},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "variable type mismatch",
					Query: rel.Clauses{
						v("entity").AttrEq(pi8, int64(0)),
					},
					ErrorRE: `failed to construct query: failed to process invalid clause \$entity\[pi8\] = 0: int64 is not int8`,
				},
				{
					// Note here that the value for e1 is implied by the binding of
					// n1 which allows the query engine to avoid making another join
					// against the database.
					Name: "entity bound via variable",
					Query: rel.Clauses{
						v("n1").AttrEqVar(value, "e1"),
						v("e1").AttrEq(pi8, int8(1)),
						v("n2").AttrEqVar(value, "e2"),
						v("i16").Entities(i16, "e1", "e2"),
					},
					Entities: []v{"n1", "e1", "n2", "e2"},
					ResVars:  []v{"n1", "e1", "n2", "e2"},
					Results: [][]interface{}{
						{na, a, na, a},
						{na, a, nc, c},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "entity bound via variable with ne filter",
					Query: rel.Clauses{
						v("n1").AttrEqVar(value, "e1"),
						v("e1").AttrEq(pi8, int8(1)),
						v("n2").AttrEqVar(value, "e2"),
						v("i16").Entities(i16, "e1", "e2"),
						rel.Filter("neq", "e1", "e2")(func(
							a, b interface{},
						) bool {
							return a != b
						}),
					},
					Entities: []v{"n1", "e1", "n2", "e2"},
					ResVars:  []v{"n1", "e1", "n2", "e2"},
					Results: [][]interface{}{
						{na, a, nc, c},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "any value type mismatch",
					Query: rel.Clauses{
						v("value").AttrIn(i8, int8(1), int8(2), int16(1)),
					},
					ErrorRE: `failed to process invalid clause \$value\[i8\] IN \[1, 2, 1\]: int16 is not int8`,
				},
				// TODO(ajwerner): This points at a real wart: we should detect the
				// type mismatch by propagating the type constraint on i8.
				{
					Name: "any clause no match on variable eq with type mismatch",
					Query: rel.Clauses{
						v("e").AttrEqVar(i8, "i8"),
						v("i8").In(1, 2),
					},
					Entities:             []v{"e"},
					ResVars:              []v{"e", "i8"},
					Results:              [][]interface{}{},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "pointer scalar values any",
					Query: rel.Clauses{
						v("e").AttrIn(i8, newInt8(1), newInt8(2)),
					},
					Entities: []v{"e"},
					ResVars:  []v{"e"},
					Results: [][]interface{}{
						{a}, {b}, {c},
					},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "pointer scalar values",
					Query: rel.Clauses{
						v("e").AttrEq(i8, newInt8(1)),
					},
					Entities: []v{"e"},
					ResVars:  []v{"e"},
					Results: [][]interface{}{
						{a},
					},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "nil pointer scalar values any",
					Query: rel.Clauses{
						v("e").AttrIn(i8, int8(1), newInt8(1), (*int8)(nil)),
					},
					ErrorRE: `failed to process invalid clause \$e\[i8\] IN \[1, 1, null\]: invalid nil \*int8`,
				},
				{
					Name: "nil pointer scalar",
					Query: rel.Clauses{
						v("e").AttrEq(i8, (*int8)(nil)),
					},
					ErrorRE: `failed to process invalid clause \$e\[i8\] = null: invalid nil \*int8`,
				},
				{
					Name: "no match in any expr",
					Query: rel.Clauses{
						v("e").AttrIn(i8, newInt8(4), newInt8(5)),
					},
					Entities:             []v{"e"},
					ResVars:              []v{"e"},
					Results:              [][]interface{}{},
					UnsatisfiableIndexes: []int{2, 3, 4, 5, 6},
				},
				{
					Name: "any clause no match on variable eq",
					Query: rel.Clauses{
						v("e").AttrEqVar(i8, "i8"),
						v("i8").In(int8(3), int8(4)),
					},
					Entities:             []v{"e"},
					ResVars:              []v{"e", "i8"},
					Results:              [][]interface{}{},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "using blank, bind all",
					Query: rel.Clauses{
						v("e").AttrEqVar(i8, "_"),
					},
					Entities: []v{"e"},
					ResVars:  []v{"e"},
					Results: [][]interface{}{
						{a}, {b}, {c},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
				{
					Name: "using blank, bind non-nil pointer",
					Query: rel.Clauses{
						v("e").AttrEqVar(pi8, "_"),
					},
					Entities: []v{"e"},
					ResVars:  []v{"e"},
					Results: [][]interface{}{
						{a},
					},
					UnsatisfiableIndexes: []int{1, 2, 3, 4, 5, 6},
				},
			},
		},
	}
	attributeCases = []reltest.AttributeTestCase{
		{
			Entity: "a",
			Expected: addToEmptyEntityMap(map[rel.Attr]interface{}{
				pi8: int8(1),
				i8:  int8(1),
				i16: int16(1),
			}),
		},
		{
			Entity: "b",
			Expected: addToEmptyEntityMap(map[rel.Attr]interface{}{
				i8:  int8(2),
				i16: int16(2),
			}),
		},
		{
			Entity: "c",
			Expected: addToEmptyEntityMap(map[rel.Attr]interface{}{
				i8:  int8(2),
				i16: int16(1),
			}),
		},
		{
			Entity: "na",
			Expected: map[rel.Attr]interface{}{
				value: a,
			},
		},
		{
			Entity: "nb",
			Expected: map[rel.Attr]interface{}{
				value: b,
				left:  na,
			},
		},
		{
			Entity: "nc",
			Expected: map[rel.Attr]interface{}{
				value: c,
				right: nb,
			},
		},
	}
)

func newInt8(i int8) *int8 { return &i }

func addToEmptyEntityMap(m map[rel.Attr]interface{}) map[rel.Attr]interface{} {
	base := map[rel.Attr]interface{}{
		i8:  int8(0),
		i16: int16(0),
	}
	for k, v := range m {
		base[k] = v
	}
	return base
}
