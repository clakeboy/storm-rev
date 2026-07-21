package storm

import (
	"context"
	"reflect"
	"testing"

	"github.com/clakeboy/storm-rev/v2/q"
	"github.com/stretchr/testify/require"
)

type selectIndexPlanUser struct {
	ID    int    `storm:"id"`
	Name  string `storm:"index"`
	Age   int    `storm:"index,composite=group_age:2"`
	Group string `storm:"index,composite=group_age:1"`
	Note  string
}

type selectIndexedScore struct {
	ID    int `storm:"id,increment"`
	Value int `storm:"index"`
	Rank  int
	Group string `storm:"index"`
}

func selectIndexPlanConfig(t *testing.T) *structConfig {
	t.Helper()

	value := reflect.ValueOf(&selectIndexPlanUser{})
	cfg, err := extract(&value)
	require.NoError(t, err)
	return cfg
}

func explainedPlan(t *testing.T, matcher q.Matcher) (*selectIndexPlan, bool) {
	t.Helper()

	expr, ok := q.Explain(matcher)
	require.True(t, ok)
	return selectIndexPlanForConfig(selectIndexPlanConfig(t), expr)
}

func TestSelectIndexPlanSingleFieldExact(t *testing.T) {
	plan, ok := explainedPlan(t, q.Eq("Name", "John"))
	require.True(t, ok)
	require.Equal(t, selectIndexPlanExact, plan.kind)
	require.Equal(t, "Name", plan.field)
}

func TestSelectIndexPlanRange(t *testing.T) {
	plan, ok := explainedPlan(t, q.And(q.Gte("Age", 10), q.Lte("Age", 20)))
	require.True(t, ok)
	require.Equal(t, selectIndexPlanRange, plan.kind)
	require.Equal(t, "Age", plan.field)
}

func TestSelectIndexPlanCompositePreferred(t *testing.T) {
	plan, ok := explainedPlan(t, q.And(q.Eq("Group", "staff"), q.Eq("Age", 20), q.Eq("Name", "John")))
	require.True(t, ok)
	require.Equal(t, selectIndexPlanComposite, plan.kind)
	require.Equal(t, "group_age", plan.indexName)
	require.Equal(t, []any{"staff", 20}, plan.values)
}

func TestSelectIndexPlanOrRequiresIndexedBranches(t *testing.T) {
	plan, ok := explainedPlan(t, q.Or(q.Eq("Name", "John"), q.Eq("Age", 20)))
	require.True(t, ok)
	require.Equal(t, selectIndexPlanUnion, plan.kind)
	require.Len(t, plan.children, 2)

	_, ok = explainedPlan(t, q.Or(q.Eq("Name", "John"), q.Eq("Note", "private")))
	require.False(t, ok)
}

func TestSelectIndexPlanRejectsUnsupportedMatchers(t *testing.T) {
	_, ok := explainedPlan(t, q.Not(q.Eq("Name", "John")))
	require.False(t, ok)

	_, ok = explainedPlan(t, q.Re("Name", "^Jo"))
	require.False(t, ok)

	_, ok = explainedPlan(t, q.Eq("Note", "private"))
	require.False(t, ok)
}

func prepareSelectIndexedScoreDB(t *testing.T) (*DB, func()) {
	t.Helper()

	db, cleanup := createDB(t)
	for i := 0; i < 20; i++ {
		err := db.Save(&selectIndexedScore{
			Value: i,
			Rank:  20 - i,
			Group: "staff",
		})
		require.NoError(t, err)
	}
	require.NoError(t, db.FlushBleve(context.Background()))
	return db, cleanup
}

func selectIndexedScoreMatcher() q.Matcher {
	return q.Or(
		q.Eq("Value", 5),
		q.And(q.Gte("Value", 1), q.Lte("Value", 2)),
		q.And(q.Gte("Value", 18), q.Lte("Value", 19)),
	)
}

func TestSelectUsesIndexCandidatesForFindFirstCount(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	var scores []selectIndexedScore
	err := db.Select(selectIndexedScoreMatcher()).Skip(2).Limit(3).Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 3)
	require.Equal(t, []int{5, 18, 19}, []int{scores[0].Value, scores[1].Value, scores[2].Value})

	var first selectIndexedScore
	err = db.Select(selectIndexedScoreMatcher()).Reverse().Skip(1).First(&first)
	require.NoError(t, err)
	require.Equal(t, 18, first.Value)

	total, err := db.Select(selectIndexedScoreMatcher()).Skip(2).Limit(2).Count(&selectIndexedScore{})
	require.NoError(t, err)
	require.Equal(t, 2, total)
}

func TestSelectIndexCandidatesPreserveOrderBy(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	var scores []selectIndexedScore
	err := db.Select(q.Eq("Group", "staff")).OrderBy("Rank").Limit(3).Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 3)
	require.Equal(t, []int{19, 18, 17}, []int{scores[0].Value, scores[1].Value, scores[2].Value})

	scores = nil
	err = db.Select(q.Eq("Group", "staff")).OrderBy("Rank").Reverse().Skip(1).Limit(2).Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 2)
	require.Equal(t, []int{1, 2}, []int{scores[0].Value, scores[1].Value})
}

func TestSelectIndexCandidatesFilterNonIndexedConditions(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	var scores []selectIndexedScore
	err := db.Select(q.Eq("Group", "staff"), q.Gte("Rank", 18)).Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 3)
	require.Equal(t, []int{0, 1, 2}, []int{scores[0].Value, scores[1].Value, scores[2].Value})
}

func TestSelectDeleteUpdatesIndexFromCandidates(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	err := db.Select(q.Eq("Value", 5)).Delete(&selectIndexedScore{})
	require.NoError(t, err)
	require.NoError(t, db.FlushBleve(context.Background()))
	require.False(t, db.indexer.isDirty("selectIndexedScore"))

	var scores []selectIndexedScore
	err = db.Find("Value", 5, &scores)
	require.Equal(t, ErrNotFound, err)
	require.False(t, db.indexer.isDirty("selectIndexedScore"))

	err = db.Select(q.And(q.Gte("Value", 4), q.Lte("Value", 6))).OrderBy("Value").Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 2)
	require.Equal(t, []int{4, 6}, []int{scores[0].Value, scores[1].Value})
	require.False(t, db.indexer.isDirty("selectIndexedScore"))
}

func TestSelectFallsBackWhenIndexDirty(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	db.indexer.markDirty("selectIndexedScore")

	var scores []selectIndexedScore
	err := db.Select(q.Eq("Value", 5)).Find(&scores)
	require.NoError(t, err)
	require.Len(t, scores, 1)
	require.Equal(t, 5, scores[0].Value)
}

func TestSelectFallsBackForZeroValueMatcher(t *testing.T) {
	db, cleanup := prepareSelectIndexedScoreDB(t)
	defer cleanup()

	var score selectIndexedScore
	err := db.Select(q.Eq("Value", 0)).First(&score)
	require.NoError(t, err)
	require.Equal(t, 0, score.Value)
}
