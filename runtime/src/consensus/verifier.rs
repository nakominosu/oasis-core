//! Trait for consensus layer verification.
use anyhow::anyhow;
use io_context::Context;
use thiserror::Error;

use super::{
    beacon::EpochTime,
    roothash::{ComputeResultsHeader, Header},
    state::{registry::ImmutableState as RegistryState, ConsensusState},
    LightBlock,
};
use crate::{
    common::{crypto::signature::PublicKey, namespace::Namespace, version::Version},
    rak::RAK,
    types,
};

#[derive(Debug, Error)]
pub enum Error {
    #[error("builder: {0}")]
    Builder(#[source] anyhow::Error),

    #[error("verification: {0}")]
    VerificationFailed(#[source] anyhow::Error),

    #[error("trust root loading failed")]
    TrustRootLoadingFailed,

    #[error("internal consensus verifier error")]
    Internal,
}

impl Error {
    fn code(&self) -> u32 {
        match self {
            Error::Builder(_) => 1,
            Error::VerificationFailed(_) => 2,
            Error::TrustRootLoadingFailed => 3,
            Error::Internal => 4,
        }
    }
}

impl From<Error> for types::Error {
    fn from(e: Error) -> Self {
        Self {
            module: "verifier".to_string(),
            code: e.code(),
            message: e.to_string(),
        }
    }
}

/// Verifier is the consensus layer state verifier trait.
pub trait Verifier: Send + Sync {
    /// Synchronize the verifier state up to including the passed consensus height.
    fn sync(&self, height: u64) -> Result<(), Error>;

    /// Verify that the given runtime header is valid at the given consensus layer block and return
    /// the consensus layer state accessor for that block.
    ///
    /// This also verifies that the state is fresh.
    fn verify(
        &self,
        consensus_block: LightBlock,
        runtime_header: Header,
        epoch: EpochTime,
    ) -> Result<ConsensusState, Error>;

    /// Verify that the given runtime header is valid at the given consensus layer block and return
    /// the consensus layer state accessor for that block.
    ///
    /// This is a relaxed version of the `verify` function that should be used for verifying state
    /// in queries.
    fn verify_for_query(
        &self,
        consensus_block: LightBlock,
        runtime_header: Header,
        epoch: EpochTime,
    ) -> Result<ConsensusState, Error>;

    /// Return the consensus layer state accessor for the given consensus layer block WITHOUT
    /// performing any verification. This method should only be used for operations that do not
    /// require integrity guarantees.
    fn unverified_state(&self, consensus_block: LightBlock) -> Result<ConsensusState, Error>;

    /// Return the latest verified consensus layer state.
    ///
    /// # Warning
    ///
    /// The state is not verified to be fresh. Use `verify_state_freshness` to perform this
    /// verification manually if needed.
    fn latest_state(&self) -> Result<ConsensusState, Error>;

    /// Return the verified consensus layer state for a given height.
    ///
    /// # Warning
    ///
    /// The state is not verified to be fresh. Use `verify_state_freshness` to perform this
    /// verification manually if needed.
    fn state_at(&self, height: u64) -> Result<ConsensusState, Error>;

    /// Return the latest known consensus layer height.
    fn latest_height(&self) -> Result<u64, Error>;

    /// Record the given (locally computed and thus verified) results header as trusted.
    fn trust(&self, header: &ComputeResultsHeader) -> Result<(), Error>;
}

/// Consensus layer trust root.
#[derive(Debug, Clone, Default, PartialEq, Eq, cbor::Encode, cbor::Decode)]
pub struct TrustRoot {
    /// Known trusted height.
    pub height: u64,
    /// Known hex-encoded trusted consensus layer header hash.
    pub hash: String,
    /// Known runtime identifier.
    pub runtime_id: Namespace,
}

/// Verify consensus layer state freshness based on our internal state.
///
/// Returns the node ID of the node where this runtime is executing on. The same node ID may be
/// passed in order to optimize discovery for subsequent runs.
pub fn verify_state_freshness(
    state: &ConsensusState,
    trust_root: &TrustRoot,
    rak: &RAK,
    version: &Version,
    node_id: &Option<PublicKey>,
) -> Result<Option<PublicKey>, Error> {
    let registry_state = RegistryState::new(&state);

    match node_id {
        // Node ID is cached, query the node and check for matching RAK.
        Some(node_id) => {
            let node = registry_state
                .node(Context::background(), node_id)
                .map_err(|err| {
                    Error::VerificationFailed(anyhow!(
                        "failed to retrieve node from the registry: {}",
                        err
                    ))
                })?;
            let node = node.ok_or_else(|| {
                Error::VerificationFailed(anyhow!(
                    "own node ID '{}' not found in registry state",
                    node_id,
                ))
            })?;
            if !node.has_tee(rak, &trust_root.runtime_id, version) {
                return Err(Error::VerificationFailed(anyhow!(
                    "own RAK not found in registry state"
                )));
            }

            Ok(Some(*node_id))
        }
        // Node ID not cached, need to scan all registry nodes.
        None => {
            let nodes = registry_state.nodes(Context::background()).map_err(|err| {
                Error::VerificationFailed(anyhow!(
                    "failed to retrieve nodes from the registry: {}",
                    err
                ))
            })?;
            let mut found_node: Option<PublicKey> = None;
            for node in nodes {
                if node.has_tee(rak, &trust_root.runtime_id, version) {
                    found_node = Some(node.id);
                    break;
                }
            }
            if found_node.is_none() {
                return Err(Error::VerificationFailed(anyhow!(
                    "own RAK not found in registry state",
                )));
            }

            // Cache node ID to avoid re-scanning.
            Ok(found_node)
        }
    }
}
