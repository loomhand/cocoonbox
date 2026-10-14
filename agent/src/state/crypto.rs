//! Chunk ids and sealing: HMAC-SHA256 ids, zstd, AES-256-GCM bound to the id.

use std::io::Read;

use aes_gcm::Aes256Gcm;
use aes_gcm::aead::{Aead, Generate, KeyInit, Nonce, Payload};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use zeroize::Zeroizing;

const NONCE_LEN: usize = 12;
const ZSTD_LEVEL: i32 = 3;
const ID_LABEL: &[u8] = b"cocoonbox state chunk id v1";
const SEAL_LABEL: &[u8] = b"cocoonbox state seal v1";

type HmacSha256 = Hmac<Sha256>;

/// Keys derived from one user key: one for chunk ids, one for encryption; both are wiped on drop.
pub struct Keys {
    id: Zeroizing<[u8; 32]>,
    seal: Aes256Gcm,
}

impl Keys {
    /// Derives the id and sealing keys from `key`, which must be at least 32 bytes.
    pub fn derive(key: &[u8]) -> Result<Self, String> {
        if key.len() < 32 {
            return Err(format!(
                "state key is {} bytes; need at least 32",
                key.len()
            ));
        }
        let seal_key = Zeroizing::new(mac(key, SEAL_LABEL)?);
        Ok(Self {
            id: Zeroizing::new(mac(key, ID_LABEL)?),
            seal: Aes256Gcm::new_from_slice(seal_key.as_slice())
                .map_err(|e| format!("seal key: {e}"))?,
        })
    }

    /// Hex HMAC of `plaintext`: the chunk's name in the store.
    pub fn id(&self, plaintext: &[u8]) -> Result<String, String> {
        Ok(hex::encode(mac(self.id.as_slice(), plaintext)?))
    }

    /// Compresses and encrypts `plaintext`; the id is authenticated as associated data.
    pub fn seal(&self, id: &str, plaintext: &[u8]) -> Result<Vec<u8>, String> {
        let packed =
            zstd::bulk::compress(plaintext, ZSTD_LEVEL).map_err(|e| format!("compress: {e}"))?;
        let nonce = Nonce::<Aes256Gcm>::generate();
        let sealed = self
            .seal
            .encrypt(
                &nonce,
                Payload {
                    msg: &packed,
                    aad: id.as_bytes(),
                },
            )
            .map_err(|_| "encrypt failed".to_owned())?;
        let mut out = Vec::with_capacity(NONCE_LEN + sealed.len());
        out.extend_from_slice(&nonce);
        out.extend_from_slice(&sealed);
        Ok(out)
    }

    /// Decrypts, decompresses at most `limit` bytes, and checks the plaintext against `id`.
    pub fn open(&self, id: &str, sealed: &[u8], limit: u64) -> Result<Vec<u8>, String> {
        let (nonce, body) = sealed
            .split_first_chunk::<NONCE_LEN>()
            .ok_or_else(|| format!("object {id} is truncated"))?;
        let packed = self
            .seal
            .decrypt(
                &Nonce::<Aes256Gcm>::from(*nonce),
                Payload {
                    msg: body,
                    aad: id.as_bytes(),
                },
            )
            .map_err(|_| format!("object {id} failed authentication"))?;
        let mut plain = Vec::new();
        zstd::stream::read::Decoder::new(packed.as_slice())
            .map_err(|e| format!("decompress {id}: {e}"))?
            .take(limit + 1)
            .read_to_end(&mut plain)
            .map_err(|e| format!("decompress {id}: {e}"))?;
        if plain.len() as u64 > limit {
            return Err(format!("object {id} exceeds {limit} bytes"));
        }
        if self.id(&plain)? != id {
            return Err(format!("object {id} does not match its id"));
        }
        Ok(plain)
    }
}

fn mac(key: &[u8], data: &[u8]) -> Result<[u8; 32], String> {
    let mut m = <HmacSha256 as Mac>::new_from_slice(key).map_err(|e| format!("hmac key: {e}"))?;
    m.update(data);
    Ok(m.finalize().into_bytes().into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn seal_round_trips_and_rejects_tampering_and_swaps() {
        let keys = Keys::derive(&[7u8; 32]).unwrap();
        let data = b"hello hello hello hello".repeat(100);
        let id = keys.id(&data).unwrap();
        assert_eq!(id.len(), 64);
        let sealed = keys.seal(&id, &data).unwrap();
        assert!(sealed.len() < data.len());
        assert_eq!(keys.open(&id, &sealed, 1 << 20).unwrap(), data);

        let mut flipped = sealed.clone();
        let last = flipped.len() - 1;
        flipped[last] ^= 1;
        assert!(
            keys.open(&id, &flipped, 1 << 20)
                .unwrap_err()
                .contains("authentication")
        );

        let other = keys.id(b"other").unwrap();
        assert!(keys.open(&other, &sealed, 1 << 20).is_err());
        assert!(keys.open(&id, &sealed, 10).unwrap_err().contains("exceeds"));

        let stranger = Keys::derive(&[8u8; 32]).unwrap();
        assert_ne!(stranger.id(&data).unwrap(), id);
        assert!(stranger.open(&id, &sealed, 1 << 20).is_err());
        assert!(Keys::derive(&[1u8; 16]).is_err());
    }
}
