package support

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/alcionai/corso/src/internal/tester"
)

type GraphConnectorErrorSuite struct {
	tester.Suite
}

func TestGraphConnectorErrorSuite(t *testing.T) {
	suite.Run(t, &GraphConnectorErrorSuite{Suite: tester.NewUnitSuite(t)})
}

func (suite *GraphConnectorErrorSuite) TestWrapAndAppend() {
	t := suite.T()

	err1 := fmt.Errorf("New Error")
	err2 := errors.New("I have two")
	returnErr := WrapAndAppend("arc376", err2, err1)
	assert.True(t, strings.Contains(returnErr.Error(), "arc376"))
	assert.Error(t, returnErr)

	multi := &multierror.Error{Errors: []error{err1, err2}}
	assert.True(t, strings.Contains(ListErrors(*multi), "two")) // Does not contain the wrapped information
	t.Log(ListErrors(*multi))
}

func (suite *GraphConnectorErrorSuite) TestWrapAndAppend_OnVar() {
	var (
		err1 error
		id   = "xi2058"
	)

	received := WrapAndAppend(id, errors.New("network error"), err1)
	assert.True(suite.T(), strings.Contains(received.Error(), id))
}

func (suite *GraphConnectorErrorSuite) TestWrapAndAppend_Add3() {
	t := suite.T()

	errOneTwo := WrapAndAppend("user1", assert.AnError, assert.AnError)
	combined := WrapAndAppend("unix36", assert.AnError, errOneTwo)
	allErrors := WrapAndAppend("fxi92874", assert.AnError, combined)
	assert.True(t, strings.Contains(combined.Error(), "unix36"))
	assert.True(t, strings.Contains(combined.Error(), "user1"))
	assert.True(t, strings.Contains(allErrors.Error(), "fxi92874"))
}

func (suite *GraphConnectorErrorSuite) TestWrapAndAppendf() {
	err1 := assert.AnError
	err2 := assert.AnError
	combined := WrapAndAppendf(134323, err2, err1)
	assert.True(suite.T(), strings.Contains(combined.Error(), "134323"))
}

func (suite *GraphConnectorErrorSuite) TestConcatenateStringFromPointers() {
	var (
		outString string
		v1        = "Corso"
		v3        = "remains"
		s1        = &v1
		s2        *string
		s3        = &v3
		t         = suite.T()
	)

	outString = concatenateStringFromPointers(outString, []*string{s1, s2, s3})
	assert.True(t, strings.Contains(outString, v1))
	assert.True(t, strings.Contains(outString, v3))
}

func (suite *GraphConnectorErrorSuite) TestGetNumberOfErrors() {
	table := []struct {
		name     string
		errs     error
		expected int
	}{
		{
			name:     "No error",
			errs:     nil,
			expected: 0,
		},
		{
			name:     "Not an ErrorList",
			errs:     errors.New("network error"),
			expected: 1,
		},
		{
			name:     "Three Errors",
			errs:     WrapAndAppend("tres", errors.New("three"), WrapAndAppend("arc376", errors.New("one"), errors.New("two"))),
			expected: 3,
		},
	}
	for _, test := range table {
		suite.Run(test.name, func() {
			result := GetNumberOfErrors(test.errs)
			assert.Equal(suite.T(), result, test.expected)
		})
	}
}
