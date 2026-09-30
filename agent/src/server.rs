//! gRPC surface: Agents and Session behind the edge-token check.

use std::sync::Arc;
use std::time::Duration;

use tokio::net::TcpListener;
use tokio::sync::{broadcast, mpsc};
use tokio_stream::wrappers::{ReceiverStream, TcpListenerStream};
use tonic::service::interceptor::InterceptedService;
use tonic::service::{Interceptor, Routes};
use tonic::transport::Server;
use tonic::{Request, Response, Status, Streaming};

use crate::config::ServeConfig;
use crate::llm;
use crate::mcp::Mcp;
use crate::pb::agents_server::{Agents, AgentsServer};
use crate::pb::session_server::{Session, SessionServer};
use crate::pb::{self, Event};
use crate::runner::Runtime;
use crate::screen;
use crate::toolhost::rpc;

/// Header the edge adds to every forwarded call.
pub const EDGE_TOKEN_HEADER: &str = "x-cocoonbox-edge-token";
const MAX_REQUEST_BYTES: usize = 64 << 20;
const DEFAULT_PAGE: u32 = 100;
const MAX_PAGE: u32 = 1000;
const EVENT_BUFFER: usize = 256;
const SCREEN_WIDTH: u32 = 1280;
const SCREEN_HEIGHT: u32 = 800;
const CONNECTION_GRACE: Duration = Duration::from_millis(1000);
const DRAIN_LIMIT: Duration = Duration::from_millis(700);

/// Rejects calls whose edge token is missing or wrong.
#[derive(Clone)]
pub struct EdgeAuth {
    token: Arc<str>,
}

impl Interceptor for EdgeAuth {
    fn call(&mut self, req: Request<()>) -> Result<Request<()>, Status> {
        let got = req
            .metadata()
            .get(EDGE_TOKEN_HEADER)
            .map(|v| v.as_bytes())
            .unwrap_or_default();
        if constant_time_eq(got, self.token.as_bytes()) {
            Ok(req)
        } else {
            Err(Status::unauthenticated("missing or invalid edge token"))
        }
    }
}

/// `cocoonbox.agent.v1.Agents`.
pub struct AgentsSvc {
    rt: Arc<Runtime>,
}

#[tonic::async_trait]
impl Agents for AgentsSvc {
    async fn create(
        &self,
        req: Request<pb::CreateAgentRequest>,
    ) -> Result<Response<pb::Agent>, Status> {
        self.rt
            .create_agent(req.into_inner())
            .await
            .map(Response::new)
    }

    async fn list(
        &self,
        _req: Request<pb::ListAgentsRequest>,
    ) -> Result<Response<pb::ListAgentsResponse>, Status> {
        let agents = self
            .rt
            .journal
            .store
            .list_agents()
            .await
            .map_err(Status::internal)?;
        Ok(Response::new(pb::ListAgentsResponse { agents }))
    }

    async fn send(
        &self,
        req: Request<pb::SendRequest>,
    ) -> Result<Response<pb::SendResponse>, Status> {
        let turn_id = self.rt.send(req.into_inner()).await?;
        Ok(Response::new(pb::SendResponse { turn_id }))
    }

    async fn transcript(
        &self,
        req: Request<pb::TranscriptRequest>,
    ) -> Result<Response<pb::TranscriptPage>, Status> {
        let req = req.into_inner();
        let limit = match req.limit {
            0 => DEFAULT_PAGE,
            n => n.min(MAX_PAGE),
        };
        self.rt
            .journal
            .store
            .transcript(req.agent_id, req.after_seq, limit)
            .await
            .map(Response::new)
            .map_err(Status::internal)
    }

    async fn respond(
        &self,
        req: Request<pb::RespondRequest>,
    ) -> Result<Response<pb::RespondResponse>, Status> {
        self.rt.respond(req.into_inner())?;
        Ok(Response::new(pb::RespondResponse {}))
    }

    async fn cancel(
        &self,
        req: Request<pb::CancelRequest>,
    ) -> Result<Response<pb::CancelResponse>, Status> {
        self.rt.cancel(&req.into_inner().agent_id);
        Ok(Response::new(pb::CancelResponse {}))
    }
}

/// `cocoonbox.agent.v1.Session`.
pub struct SessionSvc {
    rt: Arc<Runtime>,
}

#[tonic::async_trait]
impl Session for SessionSvc {
    type EventsStream = ReceiverStream<Result<Event, Status>>;
    type ScreenStream = screen::ServerStream;

    async fn hello(
        &self,
        _req: Request<pb::HelloRequest>,
    ) -> Result<Response<pb::HelloResponse>, Status> {
        let cfg = &self.rt.cfg;
        let mut models = vec![cfg.model_main.clone()];
        if cfg.model_computer != cfg.model_main {
            models.push(cfg.model_computer.clone());
        }
        Ok(Response::new(pb::HelloResponse {
            agent_version: env!("CARGO_PKG_VERSION").to_owned(),
            screen_width: SCREEN_WIDTH,
            screen_height: SCREEN_HEIGHT,
            models,
            default_main_model: cfg.model_main.clone(),
        }))
    }

    async fn events(
        &self,
        req: Request<pb::EventsRequest>,
    ) -> Result<Response<Self::EventsStream>, Status> {
        let sub = self
            .rt
            .journal
            .bus
            .subscribe(req.into_inner().from_offset)
            .map_err(Status::out_of_range)?;
        let (tx, rx) = mpsc::channel(EVENT_BUFFER);
        let stop = self.rt.stop.clone();
        // detached pump: ends when the client goes away, falls behind the live channel, or the agent stops
        tokio::spawn(async move {
            let mut last = 0;
            for e in sub.replay {
                last = e.offset;
                if tx.send(Ok(Event::clone(&e))).await.is_err() {
                    return;
                }
            }
            let mut live = sub.live;
            loop {
                let next = tokio::select! {
                    r = live.recv() => r,
                    () = stop.cancelled() => return,
                };
                match next {
                    Ok(e) if e.offset > last => {
                        last = e.offset;
                        if tx.send(Ok(Event::clone(&e))).await.is_err() {
                            return;
                        }
                    }
                    Ok(_) => {}
                    Err(broadcast::error::RecvError::Lagged(_)) => {
                        let msg =
                            format!("event stream fell behind; reconnect with from_offset {last}");
                        let _ = tx.send(Err(Status::data_loss(msg))).await;
                        return;
                    }
                    Err(broadcast::error::RecvError::Closed) => return,
                }
            }
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }

    async fn screen(
        &self,
        req: Request<Streaming<pb::ScreenClientFrame>>,
    ) -> Result<Response<Self::ScreenStream>, Status> {
        screen::relay(
            &self.rt.cfg.vnc_addr,
            req.into_inner(),
            self.rt.stop.clone(),
        )
        .await
        .map(Response::new)
    }
}

/// Binds `AGENT_LISTEN` and serves until SIGTERM or SIGINT.
pub async fn serve(cfg: ServeConfig) -> Result<(), String> {
    let listener = TcpListener::bind(cfg.listen)
        .await
        .map_err(|e| format!("bind {}: {e}", cfg.listen))?;
    eprintln!("serve listening on {}", cfg.listen);
    let model = Arc::new(llm::Client::new(
        &cfg.gw_base_url,
        &cfg.gw_api_key,
        cfg.anthropic_messages,
    )?);
    let tools = Arc::new(rpc::Client::new(cfg.toolhost_socket.clone()));
    let rt = Runtime::open(cfg, model, tools).await?;
    let stop = rt.stop.clone();
    // detached: turns the first SIGTERM or SIGINT into a shutdown
    tokio::spawn(async move {
        shutdown_signal().await;
        eprintln!("serve: stopping");
        stop.cancel();
    });
    serve_on(listener, rt).await
}

/// Serves both services on `listener` until `shutdown` resolves.
/// Serves both services and `/mcp` on `listener` until `rt.stop` is cancelled, then winds down within a bounded time.
pub async fn serve_on(listener: TcpListener, rt: Arc<Runtime>) -> Result<(), String> {
    let auth = EdgeAuth {
        token: Arc::from(rt.cfg.edge_token.as_str()),
    };
    let agents = AgentsServer::new(AgentsSvc {
        rt: Arc::clone(&rt),
    })
    .max_decoding_message_size(MAX_REQUEST_BYTES);
    let session = SessionServer::new(SessionSvc {
        rt: Arc::clone(&rt),
    });
    let mcp = Mcp::new(Arc::clone(&rt), rt.cfg.mcp_idle);
    mcp.spawn_reaper();
    let routes = Routes::new(InterceptedService::new(agents, auth.clone()))
        .add_service(InterceptedService::new(session, auth))
        .into_axum_router()
        .merge(Arc::clone(&mcp).router());
    let server = Server::builder()
        .accept_http1(true)
        .add_routes(Routes::from(routes))
        .serve_with_incoming_shutdown(
            TcpListenerStream::new(listener),
            rt.stop.clone().cancelled_owned(),
        );
    let grace = async {
        rt.stop.cancelled().await;
        tokio::time::sleep(CONNECTION_GRACE).await;
    };
    tokio::select! {
        r = server => r.map_err(|e| format!("grpc server: {e}"))?,
        () = grace => eprintln!("serve: connections still open after {CONNECTION_GRACE:?}; closing"),
    }
    mcp.close_all().await;
    rt.drain(DRAIN_LIMIT).await;
    Ok(())
}

async fn shutdown_signal() {
    let term = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut s) => {
                s.recv().await;
            }
            Err(_) => std::future::pending::<()>().await,
        }
    };
    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        () = term => {}
    }
}

/// Compares two secrets without an early exit.
pub(crate) fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}
