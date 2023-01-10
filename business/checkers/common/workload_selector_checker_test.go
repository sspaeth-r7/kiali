package common

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tests/data"
	"github.com/kiali/kiali/tests/testutils/validations"
)

func TestPresentWorkloads(t *testing.T) {
	assert := assert.New(t)

	validations, valid := WorkloadSelectorNoWorkloadFoundChecker(
		"sidecar",
		map[string]string{
			"app":     "details",
			"version": "v1",
		},
		workloadList(),
	).Check()

	// Well configured object
	assert.True(valid)
	assert.Empty(validations)

	validations, valid = WorkloadSelectorNoWorkloadFoundChecker(
		"sidecar",
		map[string]string{
			"app": "details",
		},
		workloadList(),
	).Check()

	// Well configured object
	assert.True(valid)
	assert.Empty(validations)
}

func TestWorkloadNotFound(t *testing.T) {
	assert := assert.New(t)
	testFailureWithWorkloadList(assert, map[string]string{"app": "wrong", "version": "v1"})
	testFailureWithWorkloadList(assert, map[string]string{"app": "details", "version": "wrong"})
	testFailureWithWorkloadList(assert, map[string]string{"app": "wrong"})
	testFailureWithEmptyWorkloadList(assert, map[string]string{"app": "wrong", "version": "v1"})
	testFailureWithEmptyWorkloadList(assert, map[string]string{"app": "details", "version": "wrong"})
	testFailureWithEmptyWorkloadList(assert, map[string]string{"app": "wrong"})
}

func testFailureWithWorkloadList(assert *assert.Assertions, selector map[string]string) {
	testFailure(assert, selector, workloadList(), "generic.selector.workloadnotfound")
}

func testFailureWithEmptyWorkloadList(assert *assert.Assertions, selector map[string]string) {
	testFailure(assert, selector, data.CreateWorkloadsPerNamespace([]string{"test"}, models.WorkloadListItem{}), "generic.selector.workloadnotfound")
}

func testFailure(assert *assert.Assertions, selector map[string]string, wl map[string]models.WorkloadList, code string) {
	vals, valid := WorkloadSelectorNoWorkloadFoundChecker(
		"sidecar",
		selector,
		wl,
	).Check()

	assert.True(valid)
	assert.NotEmpty(vals)
	assert.Len(vals, 1)
	assert.NoError(validations.ConfirmIstioCheckMessage(code, vals[0]))
	assert.Equal(vals[0].Severity, models.WarningSeverity)
	assert.Equal(vals[0].Path, "spec/workloadSelector/labels")
}
