package hub

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
)

func enrollSubjectFixture(t *testing.T, h *Hub, key ed25519.PrivateKey, id, subject, requestID string) {
	t.Helper()
	secret := strings.Repeat("a", 64)
	c, w := enrollmentRetryChallenge(t, h, id, secret, requestID)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w = enrollmentStoredRegister(t, h, enrollmentRetryGrantRequest(t, c, key, secret, subject))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestAPIEnrollmentSubjectReleaseIsExplicitAndKeepsOldPairing(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	id, subject := "subject-old-pc", "sub2api-user:601"
	enrollSubjectFixture(t, h, key, id, subject, "11111111-1111-4111-8111-111111111111")
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "Prior owner"})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "Future owner"})
	oldUser, newUser := h.managedUsers.Users[0], h.managedUsers.Users[1]
	managedChange(t, h, ManagedUserChange{Action: "assign", ID: oldUser.ID, DeviceRef: adminRef(deviceAccount(id), id)})
	h.mu.Lock()
	d := h.accounts[deviceAccount(id)].devices[id]
	ref := d.enrollment.subjectRef()
	pairHash := sha256.Sum256([]byte("synthetic-retained-phone-token"))
	h.setPairLocked(deviceAccount(id), d, pairHash, true)
	h.mu.Unlock()
	managedChange(t, h, ManagedUserChange{Action: "save", ID: oldUser.ID, Name: oldUser.Name, Disabled: true})
	base := ManagedUserChange{Action: "release-source", ID: oldUser.ID, SourceRef: ref, ExpectedRevision: h.managedUsers.Revision, Confirm: true, Reason: "Move future enrollment only"}
	for _, kind := range []string{"unconfirmed", "no-reason", "wrong-owner", "wrong-association", "single-device-move", "delete-user"} {
		t.Run(kind, func(t *testing.T) {
			in := base
			switch kind {
			case "unconfirmed":
				in.Confirm = false
			case "no-reason":
				in.Reason = ""
			case "wrong-owner":
				in.ID = newUser.ID
			case "wrong-association":
				in.SourceRef = strings.Repeat("b", 64)
			case "single-device-move":
				in.Action = "assign"
				in.ID = newUser.ID
				in.SourceRef = ""
				in.DeviceRef = adminRef(deviceAccount(id), id)
			case "delete-user":
				in.Action = "delete"
				in.SourceRef = ""
			}
			if err := h.ApplyManagedUserChange(in); err == nil {
				t.Fatal("unsafe association mutation accepted")
			}
			if h.managedUsers.EnrollmentSubjects[ref] != oldUser.ID {
				t.Fatal("failed mutation changed mapping")
			}
		})
	}
	view, _ := json.Marshal(h.ManagedUsersSnapshot())
	if strings.Contains(string(view), subject) || strings.Contains(string(view), strings.Repeat("a", 64)) || !strings.Contains(string(view), ref) {
		t.Fatal("admin view leaks subject/secret or omits opaque association")
	}
	managedChange(t, h, ManagedUserChange{Action: "release-source", ID: oldUser.ID, SourceRef: ref})
	if owner, known := h.managedUsers.EnrollmentSubjects[ref]; !known || owner != "" {
		t.Fatal("release did not retain tombstone")
	}
	if h.managedUsers.Devices[id].UserID != oldUser.ID || !h.userDisabledLocked(id) || !d.hasPair || d.pairHash != pairHash {
		t.Fatal("release altered existing owner/disablement/phone")
	}
	last := h.adminAudit[len(h.adminAudit)-1]
	if last.Action != "user-release-source" || last.DeviceRef != ref {
		t.Fatal("release lacks scoped audit")
	}
	newID := "subject-new-pc"
	enrollSubjectFixture(t, h, key, newID, subject, "22222222-2222-4222-8222-222222222222")
	if h.managedUsers.Devices[newID].UserID != "" {
		t.Fatal("released subject inferred old owner from retained device")
	}
	managedChange(t, h, ManagedUserChange{Action: "assign", ID: newUser.ID, DeviceRef: adminRef(deviceAccount(newID), newID)})
	if h.managedUsers.EnrollmentSubjects[ref] != newUser.ID || h.managedUsers.Devices[id].UserID != oldUser.ID || !d.hasPair {
		t.Fatal("new assignment silently moved old device")
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	restored := newDeviceHub(t, h.cfg)
	old := restored.accounts[deviceAccount(id)].devices[id]
	if restored.managedUsers.EnrollmentSubjects[ref] != newUser.ID || !restored.userDisabledLocked(id) || !old.hasPair || old.pairHash != pairHash {
		t.Fatal("complete data restore lost association or old restrictions")
	}
}

func TestAPIEnrollmentReleasedAssociationCanReattachSameOwnerWithoutRePair(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	id := "subject-reattach-pc"
	enrollSubjectFixture(t, h, key, id, "sub2api-user:602", "11111111-1111-4111-8111-111111111111")
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "Same owner"})
	user := h.managedUsers.Users[0]
	assign := ManagedUserChange{Action: "assign", ID: user.ID, DeviceRef: adminRef(deviceAccount(id), id)}
	managedChange(t, h, assign)
	h.mu.Lock()
	d := h.accounts[deviceAccount(id)].devices[id]
	ref := d.enrollment.subjectRef()
	h.setPairLocked(deviceAccount(id), d, sha256.Sum256([]byte("phone")), true)
	pair := d.pairHash
	h.mu.Unlock()
	managedChange(t, h, ManagedUserChange{Action: "release-source", ID: user.ID, SourceRef: ref})
	managedChange(t, h, assign)
	if h.managedUsers.EnrollmentSubjects[ref] != user.ID || !d.hasPair || d.pairHash != pair {
		t.Fatal("same-owner reattach lost association or phone binding")
	}
}

func TestAPIEnrollmentMetadataIsChargedAndRestored(t *testing.T) {
	h, key := enrollmentRetryFixture(t)
	id := "metadata-pc"
	enrollSubjectFixture(t, h, key, id, "sub2api-user:603", "11111111-1111-4111-8111-111111111111")
	d := h.accounts[deviceAccount(id)].devices[id]
	_, base, _ := prepareDeviceMetadata(d.info)
	if d.metadataBytes != base+enrollmentMetadataCost(d.enrollment) {
		t.Fatal("unaccounted enrollment metadata")
	}
	before := h.deviceMetadataBytes
	w := deviceRequestWithoutEnrollmentClaim(t, h, "/device/register", id, strings.Repeat("a", 64), d.info)
	if w.Code != 200 || h.deviceMetadataBytes != before {
		t.Fatal("reconnect dropped/doubled enrollment charge", w.Code)
	}
	raw, _ := json.Marshal(h.snapshot())
	_, charge, err := decodeDevicesState(raw, h.deviceMetadataLimit)
	if err != nil || charge != before {
		t.Fatal("restored metadata budget differs", charge, before, err)
	}
	if _, _, err := decodeDevicesState(raw, before-1); err == nil {
		t.Fatal("restore bypassed metadata limit")
	}
}
