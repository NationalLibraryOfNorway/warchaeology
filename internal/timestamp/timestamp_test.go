// Copyright 2020 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package timestamp_test

import (
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

type TestData struct {
	iso8601Date  string
	gowarc14Date string
	invalidDate  string
}

func createTestData() TestData {
	return TestData{
		iso8601Date:  "2020-01-05T10:44:25Z",
		gowarc14Date: "20200105104425",
		invalidDate:  "ThisIsNotADate20200303",
	}
}

func TestTo14SucceedsOnValidString(t *testing.T) {
	data := createTestData()

	output, err := timestamp.To14(data.iso8601Date)
	if err != nil {
		t.Errorf("Error on valid input, err: %s", err)
	}

	if output != data.gowarc14Date {
		t.Errorf("Unexpected output date %s", data.gowarc14Date)
	}
}

func TestTo14ErrorOnInValidString(t *testing.T) {
	data := createTestData()

	_, err := timestamp.To14(data.invalidDate)
	if err != nil {
		return // Test ok
	}

	t.Errorf("Did not fail on invalid when sending %s", data.invalidDate)
}

func TestFrom14ToTimeSucceedsOnValidString(t *testing.T) {
	data := createTestData()

	_, err := timestamp.Parse(data.gowarc14Date)
	if err != nil {
		t.Errorf("Error on valid date string, err: %s", err)
	}
	// TODO: Maybe test time returned here
}
