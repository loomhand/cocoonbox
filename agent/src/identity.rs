//! Box identity and the edge credential check that admits every call into the agent.

use std::collections::HashMap;
use std::fs::{File, Metadata};
use std::io::{self, Read};
use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, PoisonError};

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ed25519_dalek::{Signature, VerifyingKey};
use serde::Deserialize;
use sha2::{Digest, Sha256};

/// Header carrying the edge-signed credential.
pub const CREDENTIAL_HEADER: &str = "x-cocoonbox-credential";
/// Header carrying the shared edge token while boxes switch to credentials.
pub const EDGE_TOKEN_HEADER: &str = "x-cocoonbox-edge-token";

const VERSION_PREFIX: &str = "v1.";
const SHARE_VERSION_PREFIX: &str = "v2.";
const SHARE_PREFIX: &str = "share:";
const EXP_SKEW_SECS: i64 = 120;
const MAX_IDENTITY_BYTES: u64 = 4096;
const KEY_ID_HEX: usize = 16;
const MALFORMED: Denied = Denied::Unauthenticated("malformed credential");

/// Why a call is refused.
#[derive(Debug, PartialEq, Eq)]
pub enum Denied {
    /// The box has no usable identity; maps to `UNAVAILABLE`.
    Unprovisioned(&'static str),
    /// The credential or token is wrong; maps to `UNAUTHENTICATED`.
    Unauthenticated(&'static str),
}

/// Who a call comes from, as the edge signed it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Principal {
    /// The box's owner: a v1 credential, or the edge token.
    Owner,
    /// A guest through share `id` (v2): `write` allows every in-box tool, `user` names the guest of a named share.
    Share {
        id: String,
        write: bool,
        user: Option<String>,
    },
}

impl Principal {
    /// Parses `share:<id>:<read|write>[:<user>]`.
    fn parse(text: &str) -> Option<Self> {
        let mut parts = text.strip_prefix(SHARE_PREFIX)?.splitn(3, ':');
        let id = parts.next().filter(|id| !id.is_empty())?.to_owned();
        let write = match parts.next()? {
            "read" => false,
            "write" => true,
            _ => return None,
        };
        let user = match parts.next() {
            Some("") => return None,
            user => user.map(str::to_owned),
        };
        Some(Self::Share { id, write, user })
    }
}

/// Admits calls from the edge: a credential once the identity file exists, the edge token before.
pub struct Gate {
    path: PathBuf,
    owner: u32,
    edge_token: Option<String>,
    inner: Mutex<Inner>,
}

impl Gate {
    /// Reads the identity at `path`, which with its directory must belong to uid `owner`.
    pub fn new(path: PathBuf, owner: u32, edge_token: Option<String>) -> Self {
        Self {
            path,
            owner,
            edge_token,
            inner: Mutex::new(Inner::default()),
        }
    }

    /// Checks the `x-cocoonbox-credential` and `x-cocoonbox-edge-token` values of one call and returns its principal.
    pub fn check(
        &self,
        credential: Option<&str>,
        edge_token: Option<&[u8]>,
    ) -> Result<Principal, Denied> {
        let now = crate::now_ms() / 1000;
        let mut inner = self.inner.lock().unwrap_or_else(PoisonError::into_inner);
        if inner.identity.is_none() {
            self.reload(&mut inner);
            if inner.identity.is_none() {
                return self.before_identity(&inner, edge_token);
            }
        }
        let credential = credential.ok_or(Denied::Unauthenticated("missing credential"))?;
        if let Some((exp, principal)) = inner.verified.get(credential) {
            return fresh(*exp, now).map(|()| principal.clone());
        }
        let cred = Credential::parse(credential)?;
        if inner.identity.as_ref().is_some_and(|id| !id.matches(&cred)) {
            self.reload(&mut inner);
        }
        let Some(identity) = &inner.identity else {
            return Err(Denied::Unprovisioned("identity invalid"));
        };
        identity.verify(&cred)?;
        fresh(cred.exp, now)?;
        inner
            .verified
            .retain(|_, (exp, _)| now <= exp.saturating_add(EXP_SKEW_SECS));
        inner
            .verified
            .insert(credential.into(), (cred.exp, cred.principal.clone()));
        Ok(cred.principal)
    }

    fn before_identity(
        &self,
        inner: &Inner,
        edge_token: Option<&[u8]>,
    ) -> Result<Principal, Denied> {
        if inner.problem.is_some() {
            return Err(Denied::Unprovisioned("identity invalid"));
        }
        match &self.edge_token {
            Some(want) if edge_token.is_some_and(|got| constant_time_eq(got, want.as_bytes())) => {
                Ok(Principal::Owner)
            }
            Some(_) => Err(Denied::Unauthenticated("missing or invalid edge token")),
            None => Err(Denied::Unprovisioned("identity not provisioned")),
        }
    }

    fn reload(&self, inner: &mut Inner) {
        inner.verified.clear();
        match load(&self.path, self.owner) {
            Ok(identity) => {
                inner.identity = identity;
                inner.problem = None;
            }
            Err(e) => {
                if inner.problem.as_deref() != Some(e.as_str()) {
                    eprintln!("identity: {e}");
                }
                inner.identity = None;
                inner.problem = Some(e);
            }
        }
    }
}

#[derive(Default)]
struct Inner {
    identity: Option<Identity>,
    verified: HashMap<Box<str>, (i64, Principal)>,
    problem: Option<String>,
}

struct Identity {
    box_id: String,
    wake_id: String,
    key_id: String,
    key: VerifyingKey,
}

impl Identity {
    fn parse(text: &str) -> Result<Self, String> {
        #[derive(Deserialize)]
        struct Wire {
            box_id: String,
            wake_id: String,
            key_id: String,
            public_key: String,
        }
        let f: Wire = serde_json::from_str(text).map_err(|e| format!("parse identity: {e}"))?;
        let raw = URL_SAFE_NO_PAD
            .decode(&f.public_key)
            .map_err(|e| format!("decode public_key: {e}"))?;
        let bytes: [u8; 32] = raw
            .try_into()
            .map_err(|_| "public_key is not 32 bytes".to_owned())?;
        let key = VerifyingKey::from_bytes(&bytes).map_err(|e| format!("public_key: {e}"))?;
        if f.key_id != key_id(&bytes) {
            return Err("key_id does not match public_key".into());
        }
        if f.box_id.is_empty() || f.wake_id.is_empty() {
            return Err("empty box_id or wake_id".into());
        }
        Ok(Self {
            box_id: f.box_id,
            wake_id: f.wake_id,
            key_id: f.key_id,
            key,
        })
    }

    fn matches(&self, c: &Credential<'_>) -> bool {
        c.key_id == self.key_id && c.wake_id == self.wake_id
    }

    fn verify(&self, c: &Credential<'_>) -> Result<(), Denied> {
        if c.box_id != self.box_id {
            return Err(Denied::Unauthenticated("credential for another box"));
        }
        if c.key_id != self.key_id {
            return Err(Denied::Unauthenticated("unknown signing key"));
        }
        if c.wake_id != self.wake_id {
            return Err(Denied::Unauthenticated("credential from another wake"));
        }
        self.key
            .verify_strict(c.signed.as_bytes(), &c.signature)
            .map_err(|_| Denied::Unauthenticated("bad credential signature"))
    }
}

/// `v1.<b64url(key_id|box_id|wake_id|exp)>.<b64url(signature over "v1.<payload>")>` from the owner,
/// or `v2.` with a fifth field, the share principal.
struct Credential<'a> {
    signed: &'a str,
    key_id: String,
    box_id: String,
    wake_id: String,
    exp: i64,
    principal: Principal,
    signature: Signature,
}

impl<'a> Credential<'a> {
    fn parse(text: &'a str) -> Result<Self, Denied> {
        let (signed, signature) = text.rsplit_once('.').ok_or(MALFORMED)?;
        let (payload, share) = match signed.strip_prefix(VERSION_PREFIX) {
            Some(p) => (p, false),
            None => (
                signed.strip_prefix(SHARE_VERSION_PREFIX).ok_or(MALFORMED)?,
                true,
            ),
        };
        let payload = URL_SAFE_NO_PAD
            .decode(payload)
            .ok()
            .and_then(|b| String::from_utf8(b).ok())
            .ok_or(MALFORMED)?;
        let mut fields = payload.split('|');
        let (Some(key_id), Some(box_id), Some(wake_id), Some(exp)) =
            (fields.next(), fields.next(), fields.next(), fields.next())
        else {
            return Err(MALFORMED);
        };
        let principal = match (share, fields.next(), fields.next()) {
            (false, None, _) => Principal::Owner,
            (true, Some(p), None) => Principal::parse(p).ok_or(MALFORMED)?,
            _ => return Err(MALFORMED),
        };
        let signature: [u8; 64] = URL_SAFE_NO_PAD
            .decode(signature)
            .ok()
            .and_then(|b| b.try_into().ok())
            .ok_or(MALFORMED)?;
        Ok(Self {
            signed,
            key_id: key_id.to_owned(),
            box_id: box_id.to_owned(),
            wake_id: wake_id.to_owned(),
            exp: exp.parse().map_err(|_| MALFORMED)?,
            principal,
            signature: Signature::from_bytes(&signature),
        })
    }
}

fn load(path: &Path, owner: u32) -> Result<Option<Identity>, String> {
    let file = match File::options()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
    {
        Ok(f) => f,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(format!("open {}: {e}", path.display())),
    };
    let dir = path.parent().unwrap_or(Path::new("/"));
    let dir_meta = dir
        .symlink_metadata()
        .map_err(|e| format!("stat {}: {e}", dir.display()))?;
    if !dir_meta.is_dir() {
        return Err(format!("{} is not a directory", dir.display()));
    }
    trusted(&dir_meta, owner).map_err(|e| format!("{}: {e}", dir.display()))?;
    let meta = file
        .metadata()
        .map_err(|e| format!("stat {}: {e}", path.display()))?;
    if !meta.is_file() {
        return Err(format!("{} is not a regular file", path.display()));
    }
    trusted(&meta, owner).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut text = String::new();
    file.take(MAX_IDENTITY_BYTES)
        .read_to_string(&mut text)
        .map_err(|e| format!("read {}: {e}", path.display()))?;
    Identity::parse(&text).map(Some)
}

fn trusted(meta: &Metadata, owner: u32) -> Result<(), String> {
    if meta.uid() != owner {
        return Err(format!("owned by uid {}, want {owner}", meta.uid()));
    }
    if meta.mode() & 0o022 != 0 {
        return Err(format!(
            "mode {:o} is group- or world-writable",
            meta.mode() & 0o7777
        ));
    }
    Ok(())
}

fn key_id(public_key: &[u8; 32]) -> String {
    let mut id = hex::encode(Sha256::digest(public_key));
    id.truncate(KEY_ID_HEX);
    id
}

fn fresh(exp: i64, now: i64) -> Result<(), Denied> {
    if now <= exp.saturating_add(EXP_SKEW_SECS) {
        Ok(())
    } else {
        Err(Denied::Unauthenticated("credential expired"))
    }
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::os::unix::fs::PermissionsExt;

    use ed25519_dalek::{Signer, SigningKey};

    use super::*;

    const TOKEN: &str = "edge-token";

    struct Fixture {
        _dir: tempfile::TempDir,
        path: PathBuf,
        owner: u32,
        key: SigningKey,
    }

    impl Fixture {
        fn new() -> Self {
            let dir = tempfile::tempdir().unwrap();
            fs::set_permissions(dir.path(), fs::Permissions::from_mode(0o755)).unwrap();
            let owner = fs::metadata(dir.path()).unwrap().uid();
            Self {
                path: dir.path().join("identity"),
                _dir: dir,
                owner,
                key: SigningKey::from_bytes(&[7; 32]),
            }
        }

        fn gate(&self, edge_token: Option<&str>) -> Gate {
            Gate::new(self.path.clone(), self.owner, edge_token.map(Into::into))
        }

        fn key_id(&self) -> String {
            key_id(&self.key.verifying_key().to_bytes())
        }

        fn write(&self, wake_id: &str) {
            let identity = serde_json::json!({
                "box_id": "cb-box",
                "wake_id": wake_id,
                "key_id": self.key_id(),
                "public_key": URL_SAFE_NO_PAD.encode(self.key.verifying_key().to_bytes()),
            });
            let tmp = self.path.with_extension("tmp");
            fs::write(&tmp, identity.to_string()).unwrap();
            fs::set_permissions(&tmp, fs::Permissions::from_mode(0o644)).unwrap();
            fs::rename(&tmp, &self.path).unwrap();
        }

        fn credential(&self, box_id: &str, wake_id: &str, exp_in: i64) -> String {
            sign(&self.key, &self.key_id(), box_id, wake_id, exp_in)
        }
    }

    fn sign(key: &SigningKey, key_id: &str, box_id: &str, wake_id: &str, exp_in: i64) -> String {
        let exp = crate::now_ms() / 1000 + exp_in;
        sign_fields(key, "v1", &format!("{key_id}|{box_id}|{wake_id}|{exp}"))
    }

    fn sign_fields(key: &SigningKey, version: &str, fields: &str) -> String {
        let signed = format!("{version}.{}", URL_SAFE_NO_PAD.encode(fields));
        let sig = URL_SAFE_NO_PAD.encode(key.sign(signed.as_bytes()).to_bytes());
        format!("{signed}.{sig}")
    }

    fn unauthenticated(reason: &'static str) -> Result<Principal, Denied> {
        Err(Denied::Unauthenticated(reason))
    }

    #[test]
    fn valid_credential_is_admitted_and_cached() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(None);
        let cred = f.credential("cb-box", "w1", 600);
        assert_eq!(gate.check(Some(&cred), None), Ok(Principal::Owner));
        assert_eq!(gate.check(Some(&cred), None), Ok(Principal::Owner));
        assert_eq!(gate.inner.lock().unwrap().verified.len(), 1);
    }

    #[test]
    fn share_credentials_carry_the_signed_principal() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(None);
        let exp = crate::now_ms() / 1000 + 600;
        let fields = |p: &str| format!("{}|cb-box|w1|{exp}|{p}", f.key_id());
        let read = sign_fields(&f.key, "v2", &fields("share:sh-1:read"));
        let reader = Ok(Principal::Share {
            id: "sh-1".into(),
            write: false,
            user: None,
        });
        assert_eq!(gate.check(Some(&read), None), reader);
        assert_eq!(gate.check(Some(&read), None), reader, "from the cache");
        let named = sign_fields(&f.key, "v2", &fields("share:sh-1:write:u-a:b"));
        assert_eq!(
            gate.check(Some(&named), None),
            Ok(Principal::Share {
                id: "sh-1".into(),
                write: true,
                user: Some("u-a:b".into())
            })
        );
        for bad in [
            sign_fields(&f.key, "v2", &format!("{}|cb-box|w1|{exp}", f.key_id())),
            sign_fields(&f.key, "v1", &fields("share:sh-1:write")),
            sign_fields(&f.key, "v2", &fields("owner")),
            sign_fields(&f.key, "v2", &fields("share:sh-1:admin")),
            sign_fields(&f.key, "v2", &fields("share::read")),
            sign_fields(&f.key, "v2", &fields("share:sh-1:read:")),
        ] {
            assert_eq!(gate.check(Some(&bad), None), Err(MALFORMED), "{bad}");
        }
        let (signed, _) = read.rsplit_once('.').unwrap();
        let write = sign_fields(&f.key, "v2", &fields("share:sh-1:write"));
        let (_, write_sig) = write.rsplit_once('.').unwrap();
        assert_eq!(
            gate.check(Some(&format!("{signed}.{write_sig}")), None),
            unauthenticated("bad credential signature")
        );
    }

    #[test]
    fn wrong_box_is_refused() {
        let f = Fixture::new();
        f.write("w1");
        let cred = f.credential("cb-other", "w1", 600);
        assert_eq!(
            f.gate(None).check(Some(&cred), None),
            unauthenticated("credential for another box")
        );
    }

    #[test]
    fn stale_wake_is_refused_after_the_file_is_rewritten() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(None);
        let old = f.credential("cb-box", "w1", 600);
        assert_eq!(gate.check(Some(&old), None), Ok(Principal::Owner));
        f.write("w2");
        let new = f.credential("cb-box", "w2", 600);
        assert_eq!(gate.check(Some(&new), None), Ok(Principal::Owner));
        assert_eq!(
            gate.check(Some(&old), None),
            unauthenticated("credential from another wake")
        );
        let never_seen = f.credential("cb-box", "w0", 600);
        assert_eq!(
            gate.check(Some(&never_seen), None),
            unauthenticated("credential from another wake")
        );
    }

    #[test]
    fn expiry_allows_two_minutes_of_skew() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(None);
        assert_eq!(
            gate.check(Some(&f.credential("cb-box", "w1", -60)), None),
            Ok(Principal::Owner)
        );
        assert_eq!(
            gate.check(Some(&f.credential("cb-box", "w1", -130)), None),
            unauthenticated("credential expired")
        );
    }

    #[test]
    fn bad_signatures_are_refused() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(None);
        let good = f.credential("cb-box", "w1", 600);
        let other = f.credential("cb-box", "w1", 601);
        let (signed, _) = good.rsplit_once('.').unwrap();
        let (_, other_sig) = other.rsplit_once('.').unwrap();
        assert_eq!(
            gate.check(Some(&format!("{signed}.{other_sig}")), None),
            unauthenticated("bad credential signature")
        );
        let attacker = SigningKey::from_bytes(&[9; 32]);
        let forged = sign(&attacker, &f.key_id(), "cb-box", "w1", 600);
        assert_eq!(
            gate.check(Some(&forged), None),
            unauthenticated("bad credential signature")
        );
        let own_key_id = key_id(&attacker.verifying_key().to_bytes());
        let self_signed = sign(&attacker, &own_key_id, "cb-box", "w1", 600);
        assert_eq!(
            gate.check(Some(&self_signed), None),
            unauthenticated("unknown signing key")
        );
    }

    #[test]
    fn malformed_and_missing_credentials_are_refused() {
        let f = Fixture::new();
        f.write("w1");
        let gate = f.gate(Some(TOKEN));
        for bad in ["", "v1", "v2.abc.def", "v1.!!.??", "v1.YQ.YQ"] {
            assert_eq!(gate.check(Some(bad), None), Err(MALFORMED), "{bad}");
        }
        assert_eq!(
            gate.check(None, Some(TOKEN.as_bytes())),
            unauthenticated("missing credential")
        );
    }

    #[test]
    fn edge_token_works_only_while_the_file_is_absent() {
        let f = Fixture::new();
        let gate = f.gate(Some(TOKEN));
        assert_eq!(
            gate.check(None, Some(TOKEN.as_bytes())),
            Ok(Principal::Owner)
        );
        assert_eq!(
            gate.check(None, Some(b"edge-tokeX")),
            unauthenticated("missing or invalid edge token")
        );
        assert_eq!(
            f.gate(None).check(None, Some(TOKEN.as_bytes())),
            Err(Denied::Unprovisioned("identity not provisioned"))
        );
        f.write("w1");
        assert_eq!(
            gate.check(None, Some(TOKEN.as_bytes())),
            unauthenticated("missing credential")
        );
    }

    #[test]
    fn untrusted_identity_files_are_refused_without_token_fallback() {
        let f = Fixture::new();
        f.write("w1");
        let cred = f.credential("cb-box", "w1", 600);
        let foreign = Gate::new(f.path.clone(), f.owner + 1, Some(TOKEN.into()));
        let invalid = Err(Denied::Unprovisioned("identity invalid"));
        assert_eq!(foreign.check(Some(&cred), None), invalid);
        assert_eq!(foreign.check(None, Some(TOKEN.as_bytes())), invalid);

        let gate = f.gate(Some(TOKEN));
        fs::set_permissions(&f.path, fs::Permissions::from_mode(0o666)).unwrap();
        assert_eq!(gate.check(Some(&cred), None), invalid);
        fs::set_permissions(&f.path, fs::Permissions::from_mode(0o644)).unwrap();
        let dir = f.path.parent().unwrap();
        fs::set_permissions(dir, fs::Permissions::from_mode(0o770)).unwrap();
        assert_eq!(gate.check(Some(&cred), None), invalid);
        fs::set_permissions(dir, fs::Permissions::from_mode(0o755)).unwrap();
        assert_eq!(gate.check(Some(&cred), None), Ok(Principal::Owner));

        let real = f.path.with_extension("real");
        fs::rename(&f.path, &real).unwrap();
        std::os::unix::fs::symlink(&real, &f.path).unwrap();
        assert_eq!(f.gate(Some(TOKEN)).check(Some(&cred), None), invalid);
    }

    #[test]
    fn identity_with_a_mismatched_key_id_is_refused() {
        let f = Fixture::new();
        let identity = serde_json::json!({
            "box_id": "cb-box",
            "wake_id": "w1",
            "key_id": "0000000000000000",
            "public_key": URL_SAFE_NO_PAD.encode(f.key.verifying_key().to_bytes()),
        });
        fs::write(&f.path, identity.to_string()).unwrap();
        fs::set_permissions(&f.path, fs::Permissions::from_mode(0o644)).unwrap();
        let cred = sign(&f.key, "0000000000000000", "cb-box", "w1", 600);
        assert_eq!(
            f.gate(None).check(Some(&cred), None),
            Err(Denied::Unprovisioned("identity invalid"))
        );
    }
}
