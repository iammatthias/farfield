//! `farfield-blobs` — the content-addressed blob store.
//!
//! An image is bytes plus derived metadata (dimensions, blurhash, dominant
//! color), keyed by a content identifier. [`BlobStore`] is the storage seam:
//! [`LocalDir`] writes to a directory (local dev), and an R2 backend can be
//! added behind the same trait for the server.
//!
//! Blobs are self-verifying: re-hashing the bytes reproduces the CID.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// Errors from blob handling.
#[derive(Debug, thiserror::Error)]
pub enum BlobError {
    /// A filesystem failure.
    #[error("io: {0}")]
    Io(#[from] std::io::Error),
    /// The bytes were not a decodable image.
    #[error("not a decodable image: {0}")]
    Image(#[from] image::ImageError),
    /// BlurHash encoding failed.
    #[error("blurhash: {0}")]
    Blurhash(String),
    /// A stored metadata sidecar was not valid JSON.
    #[error("metadata json: {0}")]
    Json(#[from] serde_json::Error),
}

/// Everything known about one blob — returned by an upload and stored
/// alongside the bytes.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BlobMeta {
    /// Content identifier — also the storage key.
    pub cid: String,
    /// Size of the bytes.
    pub size: u64,
    /// Image MIME type.
    pub mime: String,
    /// Pixel width.
    pub width: u32,
    /// Pixel height.
    pub height: u32,
    /// BlurHash placeholder string.
    pub blurhash: String,
    /// Dominant (average) color as a hex string.
    #[serde(rename = "dominantColor")]
    pub dominant_color: String,
}

/// Hash `bytes`, decode the image, and derive its metadata. The CID uses the
/// raw multicodec — a blob is opaque bytes, not a structured record.
pub fn derive_metadata(bytes: &[u8]) -> Result<BlobMeta, BlobError> {
    let cid = farfield_core::blob_cid(bytes).to_string();
    let format = image::guess_format(bytes)?;
    let mime = mime_for(format).to_string();
    let img = image::load_from_memory(bytes)?;
    let (width, height) = (img.width(), img.height());
    let rgba = img.to_rgba8();
    let blurhash = blurhash::encode(4, 3, width, height, rgba.as_raw())
        .map_err(|e| BlobError::Blurhash(e.to_string()))?;
    let dominant_color = average_color(&img.to_rgb8());
    Ok(BlobMeta {
        cid,
        size: bytes.len() as u64,
        mime,
        width,
        height,
        blurhash,
        dominant_color,
    })
}

fn mime_for(format: image::ImageFormat) -> &'static str {
    use image::ImageFormat::*;
    match format {
        Jpeg => "image/jpeg",
        Png => "image/png",
        Gif => "image/gif",
        WebP => "image/webp",
        Avif => "image/avif",
        Tiff => "image/tiff",
        Bmp => "image/bmp",
        _ => "application/octet-stream",
    }
}

fn average_color(img: &image::RgbImage) -> String {
    let (mut r, mut g, mut b, mut n) = (0u64, 0u64, 0u64, 0u64);
    for px in img.pixels() {
        r += px[0] as u64;
        g += px[1] as u64;
        b += px[2] as u64;
        n += 1;
    }
    if n == 0 {
        return "#000000".to_string();
    }
    format!(
        "#{:02x}{:02x}{:02x}",
        (r / n) as u8,
        (g / n) as u8,
        (b / n) as u8
    )
}

/// A content-addressed blob store. Implementations key on the CID.
pub trait BlobStore: Send + Sync {
    /// Store a blob's bytes and metadata.
    fn put(&self, meta: &BlobMeta, bytes: &[u8]) -> Result<(), BlobError>;
    /// Fetch a blob's bytes, if present.
    fn get_bytes(&self, cid: &str) -> Result<Option<Vec<u8>>, BlobError>;
    /// Fetch a blob's metadata, if present.
    fn get_meta(&self, cid: &str) -> Result<Option<BlobMeta>, BlobError>;
    /// Whether a blob is stored.
    fn exists(&self, cid: &str) -> Result<bool, BlobError>;
    /// Delete a blob's bytes and metadata.
    fn delete(&self, cid: &str) -> Result<(), BlobError>;
    /// Every stored blob CID.
    fn list(&self) -> Result<Vec<String>, BlobError>;
}

/// A blob store backed by a local directory — `<cid>` holds the bytes,
/// `<cid>.json` the metadata. The directory itself is the index.
pub struct LocalDir {
    root: PathBuf,
}

impl LocalDir {
    /// Open (creating if absent) a blob directory at `root`.
    pub fn open(root: impl AsRef<Path>) -> Result<Self, BlobError> {
        let root = root.as_ref().to_path_buf();
        std::fs::create_dir_all(&root)?;
        Ok(Self { root })
    }

    fn blob_path(&self, cid: &str) -> PathBuf {
        self.root.join(cid)
    }

    fn meta_path(&self, cid: &str) -> PathBuf {
        self.root.join(format!("{cid}.json"))
    }
}

impl BlobStore for LocalDir {
    fn put(&self, meta: &BlobMeta, bytes: &[u8]) -> Result<(), BlobError> {
        std::fs::write(self.blob_path(&meta.cid), bytes)?;
        std::fs::write(self.meta_path(&meta.cid), serde_json::to_vec_pretty(meta)?)?;
        Ok(())
    }

    fn get_bytes(&self, cid: &str) -> Result<Option<Vec<u8>>, BlobError> {
        match std::fs::read(self.blob_path(cid)) {
            Ok(bytes) => Ok(Some(bytes)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn get_meta(&self, cid: &str) -> Result<Option<BlobMeta>, BlobError> {
        match std::fs::read(self.meta_path(cid)) {
            Ok(bytes) => Ok(Some(serde_json::from_slice(&bytes)?)),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(e.into()),
        }
    }

    fn exists(&self, cid: &str) -> Result<bool, BlobError> {
        Ok(self.blob_path(cid).exists())
    }

    fn delete(&self, cid: &str) -> Result<(), BlobError> {
        for path in [self.blob_path(cid), self.meta_path(cid)] {
            match std::fs::remove_file(&path) {
                Ok(()) => {}
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
        }
        Ok(())
    }

    fn list(&self) -> Result<Vec<String>, BlobError> {
        let mut cids = Vec::new();
        for entry in std::fs::read_dir(&self.root)? {
            let name = entry?.file_name();
            let Some(name) = name.to_str() else { continue };
            // The bytes file is named by the bare CID; skip the .json sidecars.
            if !name.ends_with(".json") {
                cids.push(name.to_string());
            }
        }
        cids.sort();
        Ok(cids)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A tiny solid-color PNG, encoded in memory.
    fn test_png(w: u32, h: u32, color: [u8; 3]) -> Vec<u8> {
        let img = image::RgbImage::from_pixel(w, h, image::Rgb(color));
        let mut bytes = std::io::Cursor::new(Vec::new());
        image::DynamicImage::ImageRgb8(img)
            .write_to(&mut bytes, image::ImageFormat::Png)
            .unwrap();
        bytes.into_inner()
    }

    #[test]
    fn derive_metadata_reads_dimensions_and_is_content_addressed() {
        let png = test_png(8, 5, [200, 100, 50]);
        let meta = derive_metadata(&png).unwrap();
        assert_eq!((meta.width, meta.height), (8, 5));
        assert_eq!(meta.mime, "image/png");
        assert!(meta.cid.starts_with("bafk"), "raw-codec CIDv1");
        assert!(meta.dominant_color.starts_with('#'));
        assert!(!meta.blurhash.is_empty());
        // Same bytes -> same CID.
        assert_eq!(derive_metadata(&png).unwrap().cid, meta.cid);
    }

    #[test]
    fn non_image_bytes_are_rejected() {
        assert!(derive_metadata(b"this is not an image").is_err());
    }

    #[test]
    fn local_dir_round_trips_a_blob() {
        let dir = std::env::temp_dir().join(format!("farfield-blobtest-{}", std::process::id()));
        let store = LocalDir::open(&dir).unwrap();
        let png = test_png(4, 4, [10, 20, 30]);
        let meta = derive_metadata(&png).unwrap();

        store.put(&meta, &png).unwrap();
        assert!(store.exists(&meta.cid).unwrap());
        assert_eq!(store.get_bytes(&meta.cid).unwrap().unwrap(), png);
        assert_eq!(store.get_meta(&meta.cid).unwrap().unwrap(), meta);
        assert_eq!(store.list().unwrap(), vec![meta.cid.clone()]);

        store.delete(&meta.cid).unwrap();
        assert!(!store.exists(&meta.cid).unwrap());
        assert!(store.get_bytes(&meta.cid).unwrap().is_none());

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn missing_blobs_return_none_not_error() {
        let dir = std::env::temp_dir().join(format!("farfield-blobempty-{}", std::process::id()));
        let store = LocalDir::open(&dir).unwrap();
        assert!(store.get_bytes("bafkmissing").unwrap().is_none());
        assert!(store.get_meta("bafkmissing").unwrap().is_none());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
