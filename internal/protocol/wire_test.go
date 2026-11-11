package protocol

import "testing"

func TestValidateDeviceRegisterRequest(t *testing.T) {
	if err := ValidateDeviceRegisterRequest(DeviceRegisterRequest{SpaceID: "s", DeviceID: "d", Name: "laptop"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeviceRegisterRequest(DeviceRegisterRequest{SpaceID: "", DeviceID: "d", Name: "laptop"}); err == nil {
		t.Fatal("empty space accepted")
	}
	if err := ValidateDeviceRegisterRequest(DeviceRegisterRequest{SpaceID: "s", DeviceID: "d", Name: ""}); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := ValidateSpaceID("a/b"); err == nil {
		t.Fatal("path separator accepted")
	}
}
