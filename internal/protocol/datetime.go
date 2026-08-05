package protocol

import (
	"regexp"
	"strconv"
)

var strictRFC3339Pattern = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]{1,9}))?(Z|[+-]([0-9]{2}):([0-9]{2}))$`)

func validateRFC3339(field, value string) error {
	if err := requiredString(field, value); err != nil {
		return err
	}
	matches := strictRFC3339Pattern.FindStringSubmatch(value)
	if matches == nil {
		return invalidField(field, "must be an RFC3339 timestamp")
	}

	year := parseDatePart(matches[1])
	month := parseDatePart(matches[2])
	day := parseDatePart(matches[3])
	hour := parseDatePart(matches[4])
	minute := parseDatePart(matches[5])
	second := parseDatePart(matches[6])
	if month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) || hour > 23 || minute > 59 || second > 59 {
		return invalidField(field, "must be an RFC3339 timestamp")
	}

	if matches[8] != "Z" {
		offsetHour := parseDatePart(matches[9])
		offsetMinute := parseDatePart(matches[10])
		if offsetHour > 23 || offsetMinute > 59 {
			return invalidField(field, "must be an RFC3339 timestamp")
		}
	}
	return nil
}

func parseDatePart(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func daysInMonth(year, month int) int {
	switch month {
	case 2:
		if isLeapYear(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

func isLeapYear(year int) bool {
	return year%400 == 0 || (year%4 == 0 && year%100 != 0)
}
