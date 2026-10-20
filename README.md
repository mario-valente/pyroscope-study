# POC Pyroscope — Continuous Profiling com Go

POC de **continuous profiling** com [Grafana Pyroscope](https://grafana.com/oss/pyroscope/): uma app Go instrumentada com o SDK `pyroscope-go` envia profiles a cada 10s para o servidor Pyroscope, visualizados no Grafana.

## Arquitetura

```
┌─────────────┐   push (a cada 10s)   ┌──────────────┐   query    ┌──────────┐
│ app Go      │ ────────────────────► │ Pyroscope    │ ◄───────── │ Grafana  │
│ (poc-app)   │   profiles pprof      │ server :4040 │            │  :3000   │
└─────────────┘                       └──────────────┘            └──────────┘
```

## Como rodar

```bash
# 1. Sobe Pyroscope + Grafana
docker compose up -d

# 2. Roda a app (gera carga de CPU/memória/contenção e envia profiles)
cd app
go build -o poc-app .
./poc-app
```

A app envia profiles para `http://localhost:4040` (configurável via `PYROSCOPE_SERVER`).

Para parar tudo:

```bash
docker compose down        # adicione -v para apagar os dados
pkill poc-app
```

## Onde explorar

| Interface | URL | Login |
|---|---|---|
| **Grafana** (dashboard POC) | http://localhost:3000/d/pyroscope-poc | anônimo (Admin) |
| **Grafana Explore** (query livre) | http://localhost:3000/explore | anônimo |
| **Profiles Drilldown** (UI dedicada) | http://localhost:3000/a/grafana-pyroscope-app | anônimo |
| **Pyroscope UI** (UI própria do server) | http://localhost:4040 | — |

### Roteiro de exploração na UI do Grafana

1. **Explore** → datasource **Pyroscope** → escolha o *Profile type* (ex: `process_cpu / cpu`) → selecione o serviço `poc.checkout-service`. Você vê um **flamegraph** + série temporal.
2. No flamegraph, **clique num frame** (ex: `computeHash`) para dar zoom; use a busca para highlight de uma função.
3. Use o filtro por **labels**: adicione `endpoint="POST /checkout"` e compare com `GET /search` — as tags dinâmicas injetadas via `pyroscope.TagWrapper` aparecem aqui.
4. Em **Profiles Drilldown** (`/a/grafana-pyroscope-app`), explore as visões prontas: *Single view*, *Comparison view* (diff de dois períodos/seleções, útil p/ regressões) e *Favorites*.
5. **Diff de profiles**: no Explore, selecione um intervalo maior e use a comparação de dois pontos da série temporal para ver o que mudou.

## Quais dados conseguimos extrair da app

A app (`app/main.go`) envia **10 tipos de profile** (todo o pprof do Go):

| Profile type (ID) | O que mede | Caso de uso |
|---|---|---|
| `process_cpu:cpu:nanoseconds` | Tempo de CPU por função | Achar hotspots de CPU (ex: `computeHash` = ~93% aqui) |
| `memory:alloc_space:bytes` | Bytes alocados (total, mesmo se já coletado pelo GC) | Pressão de GC / churn de alocações |
| `memory:alloc_objects:count` | Nº de objetos alocados | Alocações pequenas e frequentes |
| `memory:inuse_space:bytes` | Bytes vivos agora no heap | **Memory leaks** |
| `memory:inuse_objects:count` | Nº de objetos vivos | Leaks de objetos |
| `goroutines:goroutine:count` | Stacks de todas as goroutines | **Goroutine leaks** / onde estão bloqueadas |
| `mutex:delay:nanoseconds` | Tempo esperando locks | **Contenção de mutex** |
| `mutex:contentions:count` | Nº de contenções | Frequência de disputa por lock |
| `block:delay:nanoseconds` | Tempo bloqueado (chan, IO, select, lock) | Latência oculta / sync |
| `block:contentions:count` | Nº de eventos de bloqueio | — |

### Labels/dimensões disponíveis para filtrar e agrupar

- **Estáticas** (definidas no `pyroscope.Config.Tags`): `service_name`, `region=sa-east-1`, `environment=poc`, `version=1.0.0`
- **Dinâmicas** (por trecho de código, via `pyroscope.TagWrapper`): `endpoint=POST /checkout | GET /products | GET /search`
- **Automáticas do SDK**: `service_git_ref`, `service_repository` (se em repo git), `__session_id__`, runtime do Go

Isso permite perguntas como: *"quanto CPU o endpoint X consome vs o Y?"*, *"qual função vaza memória na versão 1.0.0?"*, *"onde as goroutines estão travadas no lock?"*.

### Exemplo real desta POC (CPU, 5 min)

```
 92.8%  main.computeHash        <- hotspot evidente
 84.0%  crypto/sha256.Sum256
 34.0%  main.checkoutHandler
 33.5%  main.productsHandler
```

## Consultando via API (sem UI)

```bash
# Tipos de profile recebidos
curl -s -X POST http://localhost:4040/querier.v1.QuerierService/LabelValues \
  -H 'Content-Type: application/json' -d '{"name":"__name__"}'
# => {"names":["block","goroutines","memory","mutex","process_cpu"]}

# Flamegraph agregado de CPU dos últimos 5 min
curl -s -X POST http://localhost:4040/querier.v1.QuerierService/SelectMergeStacktraces \
  -H 'Content-Type: application/json' -d "{
    \"profile_typeID\": \"process_cpu:cpu:nanoseconds:cpu:nanoseconds\",
    \"label_selector\": \"{service_name=\\\"poc.checkout-service\\\"}\",
    \"start\": $(($(date +%s) - 300))000,
    \"end\": $(date +%s)000
  }"
```

Também dá para baixar o profile bruto em formato pprof e abrir com `go tool pprof`:

```bash
# export via Grafana: Explore -> menu do flamegraph -> Download pprof
# ou scrape direto da app se ela expuser net/http/pprof
go tool pprof profile.pb.gz
```

## Estrutura do projeto

```
├── app/main.go                                  # app Go instrumentada (SDK pyroscope-go)
├── docker-compose.yml                           # pyroscope + grafana
└── grafana/provisioning/
    ├── datasources/pyroscope.yml                # datasource Pyroscope (uid: pyroscope)
    └── dashboards/{provider.yml, pyroscope-poc.json}  # dashboard de exemplo
```

## Notas

- O overhead do profiling é baixo (~2-5% CPU com amostragem de 100Hz), adequado para produção.
- `runtime.SetMutexProfileFraction` / `SetBlockProfileRate` precisam estar ativos para mutex/block profiles (a app já faz isso).
- O SDK faz upload a cada **10s**; a retenção padrão do server é longa e o armazenamento é comprimido por blocos.
- Alternativa ao SDK: **Grafana Alloy** coleta profiles via eBPF/pull sem instrumentar o código.
