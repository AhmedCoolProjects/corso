// Code generated by "stringer -type=Schema"; DO NOT EDIT.

package model

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[UnknownSchema-0]
	_ = x[BackupOpSchema-1]
	_ = x[RestoreOpSchema-2]
	_ = x[BackupSchema-3]
}

const _Schema_name = "UnknownSchemaBackupOpSchemaRestoreOpSchemaBackupSchema"

var _Schema_index = [...]uint8{0, 13, 27, 42, 54}

func (i Schema) String() string {
	if i < 0 || i >= Schema(len(_Schema_index)-1) {
		return "Schema(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _Schema_name[_Schema_index[i]:_Schema_index[i+1]]
}
