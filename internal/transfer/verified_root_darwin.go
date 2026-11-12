//go:build darwin && cgo

package transfer

/*
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/fcntl.h>
#include <sys/acl.h>
#include <membership.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

// These are public SDK entrypoints. No opaque ACL/flagset layout is inspected.
struct vr_acl_entry {
    uint32_t tag;
    uint32_t flags;
    uint64_t rights;
    unsigned char qualifier[16];
};
struct vr_acl_snapshot {
    uint64_t device;
    uint64_t inode;
    uint32_t uid;
    uint32_t mode;
    uint32_t flags;
    unsigned int count;
    struct vr_acl_entry entries[ACL_MAX_ENTRIES];
};

#if ACL_MAX_ENTRIES != 128
#error unsupported ACL entry bound
#endif
// Detect all set flag bits through the public query API, including unknown bits.
// Apple Libc 71bbe350 posix1e/acl_flag.c returns 0/1 for a queried bit mask.
// If a future implementation refuses a bit, classification is unsupported.
static int vr_flags(void *object, uint32_t *result) {
    acl_flagset_t flags = NULL;
    if (acl_get_flagset_np(object, &flags) != 0 || flags == NULL) return -1;
    *result = 0;
    for (unsigned int bit = 0; bit < 32; ++bit) {
        uint32_t mask = UINT32_C(1) << bit;
        int set = acl_get_flag_np(flags, (acl_flag_t)mask);
        if (set != 0 && set != 1) return -1;
        if (set) *result |= mask;
    }
    return 0;
}

// A NULL acl_get_fd_np result alone is ambiguous. The filesec query below
// distinguishes successful absence from a failed metadata/ACL read.
static int vr_read_acl(int fd, struct vr_acl_snapshot *out) {
    filesec_t sec = NULL;
    acl_t acl = NULL;
    struct stat st;
    int present = 0;
    int result = -1;
    memset(out, 0, sizeof(*out));
    sec = filesec_init();
    if (sec == NULL) goto done;
    if (fstatx_np(fd, &st, sec) != 0) goto done;
    out->device = (uint64_t)st.st_dev;
    out->inode = (uint64_t)st.st_ino;
    out->uid = st.st_uid;
    out->mode = st.st_mode;
    if (filesec_query_property(sec, FILESEC_ACL, &present) != 0) goto done;
    if (!present) { result = 0; goto done; }
    if (filesec_get_property(sec, FILESEC_ACL, &acl) != 0 || acl == NULL) goto done;
    if (acl_valid(acl) != 0 || vr_flags(acl, &out->flags) != 0) goto done;
    for (unsigned int i = 0; i <= ACL_MAX_ENTRIES; ++i) {
        acl_entry_t entry = NULL;
        acl_tag_t tag;
        acl_permset_mask_t mask;
        void *qualifier = NULL;
        errno = 0;
        // On Darwin success is 0; a validated ACL's end is -1/EINVAL.
        int got = acl_get_entry(acl, (int)i, &entry);
        if (got == -1 && errno == EINVAL) { result = 0; goto done; }
        if (got != 0 || entry == NULL || i == ACL_MAX_ENTRIES) goto done;
        if (acl_get_tag_type(entry, &tag) != 0 || acl_get_permset_mask_np(entry, &mask) != 0) goto done;
        if (vr_flags(entry, &out->entries[i].flags) != 0) goto done;
        qualifier = acl_get_qualifier(entry);
        if (qualifier == NULL) goto done;
        memcpy(out->entries[i].qualifier, qualifier, 16);
        acl_free(qualifier);
        out->entries[i].tag = tag;
        out->entries[i].rights = mask;
        out->count++;
    }
done:
    if (acl != NULL) acl_free(acl);
    if (sec != NULL) filesec_free(sec);
    return result;
}

static int vr_current_uuid(uint32_t uid, unsigned char *out) {
    uuid_t id;
    int error = mbr_uid_to_uuid((uid_t)uid, id);
    if (error != 0) return -1;
    memcpy(out, id, 16);
    return 0;
}

// Public SDK definitions must agree with the value classifier's known masks.
_Static_assert(ACL_EXTENDED_ALLOW == 1 && ACL_EXTENDED_DENY == 2, "ACL tags");
_Static_assert(ACL_FLAG_NO_INHERIT == (1 << 17), "ACL flag");
_Static_assert((ACL_ENTRY_INHERITED | ACL_ENTRY_FILE_INHERIT | ACL_ENTRY_DIRECTORY_INHERIT | ACL_ENTRY_LIMIT_INHERIT | ACL_ENTRY_ONLY_INHERIT) == 0x1f0, "ACL entry flags");
_Static_assert((ACL_READ_DATA | ACL_EXECUTE | ACL_READ_ATTRIBUTES | ACL_READ_EXTATTRIBUTES | ACL_READ_SECURITY | ACL_SYNCHRONIZE) == ((1<<1)|(1<<3)|(1<<7)|(1<<9)|(1<<11)|(1<<20)), "ACL read mask");
_Static_assert((ACL_WRITE_DATA | ACL_APPEND_DATA | ACL_DELETE | ACL_DELETE_CHILD | ACL_WRITE_ATTRIBUTES | ACL_WRITE_EXTATTRIBUTES | ACL_WRITE_SECURITY | ACL_CHANGE_OWNER) == ((1<<2)|(1<<4)|(1<<5)|(1<<6)|(1<<8)|(1<<10)|(1<<12)|(1<<13)), "ACL mutation mask");
*/
import "C"

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func openVerifiedRootDirectory(path string) (*CompletionDirectory, error) {
	d, err := OpenCompletionDirectory(path)
	if err != nil {
		return nil, ErrVerifiedRootIdentity
	}
	return d, nil
}

func duplicateVerifiedRootDirectory(d *CompletionDirectory) (*CompletionDirectory, error) {
	if d == nil || d.handle == nil {
		return nil, ErrVerifiedRootClosed
	}
	fd, err := unix.FcntlInt(d.handle.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrVerifiedRootUnsupported
	}
	return &CompletionDirectory{handle: os.NewFile(uintptr(fd), "verified-root"), identity: d.identity}, nil
}

func verifyRootHandle(file *os.File, expected CompletionFileIdentity) error {
	if file == nil {
		return ErrVerifiedRootClosed
	}
	var snap C.struct_vr_acl_snapshot
	readResult := C.vr_read_acl(C.int(file.Fd()), &snap)
	runtime.KeepAlive(file)
	if readResult != 0 {
		return ErrVerifiedRootUnsupported
	}
	if (CompletionFileIdentity{Device: uint64(snap.device), Inode: uint64(snap.inode)}) != expected {
		return ErrVerifiedRootIdentity
	}
	uid := os.Geteuid()
	if uid != os.Getuid() || uint32(snap.uid) != uint32(uid) || uint32(snap.mode)&unix.S_IFMT != unix.S_IFDIR || uint32(snap.mode)&0022 != 0 {
		return ErrVerifiedRootUnsafe
	}
	acl := verifiedRootACL{flags: uint32(snap.flags), entries: make([]verifiedRootACLEntry, int(snap.count))}
	for i := range acl.entries {
		entry := snap.entries[i]
		acl.entries[i] = verifiedRootACLEntry{tag: uint32(entry.tag), flags: uint32(entry.flags), rights: uint64(entry.rights)}
		for j := range acl.entries[i].qualifier {
			acl.entries[i].qualifier[j] = byte(entry.qualifier[j])
		}
	}
	if err := classifyVerifiedRootACL(acl, func() ([16]byte, error) {
		var id [16]byte
		var value [16]C.uchar
		if C.vr_current_uuid(C.uint32_t(uid), &value[0]) != 0 {
			return id, ErrVerifiedRootUnsupported
		}
		for i := range id {
			id[i] = byte(value[i])
		}
		return id, nil
	}); err != nil {
		return err
	}
	if uid != os.Geteuid() || uid != os.Getuid() {
		return ErrVerifiedRootUnsafe
	}
	return nil
}
