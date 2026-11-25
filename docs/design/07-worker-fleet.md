# 07. 선택형 Media Runner와 다중 실행 운영 설계

## 1. 목표

단일 작업은 `archive-media` CLI가 Control 없이 MediaOperation Port를 직접 호출한다. 여러 PC의 큐·자원·취소를 관리할 때만 Control PC 한 대가 상태와 DB를 소유하고 여러 Windows PC에서 `archive-media runner` Adapter가 렌더·STT·입고 Step을 수행한다. 초기 All-in-One 관리 구성도 `worker-main` runner 하나가 같은 PC에 있는 특수한 경우다.

Worker는 로컬 CLI를 감독하지 않는다. Codex·Claude CLI 세션은 별도 Archivist Host가 담당하며, 같은 PC에 두 서비스를 설치해도 identity, credential과 lease를 공유하지 않는다. [11. 로컬 CLI Archivist Host 설계](11-archivist-host.md)가 그 경계를 소유한다.

```text
아카이브 작업 공간 ─┐
MCP/Archivist ─┼→ Archive Control → SQLite
           │       ↑
           │       ├── local worker-main
           │       ├── worker-edit-01
           │       └── worker-ingest-01
           └──── 상태·로그·승인
```

## 2. 사용자 인증과 Worker 인증 분리

개발 단계 아카이브 작업 공간은 `auth_mode: disabled`이며 사용자별 계정이나 작업 소유자가 없다. Worker는 사람이 아니라 실행 결과를 기록하는 서비스 주체이므로 다음 값이 반드시 필요하다.

```text
worker_id
credential_ref
enabled
processing_kinds
allowed_storage_ids
heartbeat_ttl
```

사전 등록되지 않았거나 credential 검증에 실패한 Worker는 연결할 수 없다. `WRK-001`

credential 원문은 YAML에 저장하지 않는다. Control과 Worker의 로컬 Secret Store에 각각 reference를 두고 통신 Adapter가 실제 값을 가져온다. LAN을 통과하는 service credential은 TLS 또는 암호화된 overlay 안에서만 전송한다.

사용자 profile이 적재된 전용 서비스 계정은 `wincred://` generic credential을 사용할 수 있다. profile 없는 서비스 또는 OpenSSH 로그온에서 설치할 때는 `dpapi://<absolute-blob-path>`를 사용하고, 설치기가 DPAPI blob 경로의 ACL을 해당 Worker 서비스 계정과 `SYSTEM`으로 제한한다. 두 reference 모두 raw credential을 설정·argument·로그에 넣지 않는다. `CFG-007`, `LRH-010`

## 3. 시작 설정

Control 설정:

```yaml
workers:
  - id: worker-main
    enabled: true
    credential_ref: wincred://archive/workers/worker-main
    processing_kinds: [INSPECT, RENDER, CHECKSUM, DEVICE]
  - id: worker-stt-01
    enabled: true
    credential_ref: wincred://archive/workers/worker-stt-01
    processing_kinds: [INSPECT, STT]
```

Worker 설정:

```yaml
worker:
  id: worker-stt-01
  control_url: https://control.internal:8443
  credential_ref: wincred://archive/workers/worker-stt-01
  process_logs:
    root: C:/Archive/logs/operations
    max_file_bytes: 4194304
    tail_bytes: 12288
    retention: 336h
    max_total_bytes: 268435456
  storage_roots:
    - storage_id: source-main
      root: E:/
    - storage_id: working-main
      root: D:/Archive/working
  tools:
    ffmpeg:
      executable: C:/ffmpeg/bin/ffmpeg.exe
      version: ffmpeg-8.0
    ffprobe:
      executable: C:/ffmpeg/bin/ffprobe.exe
      version: ffprobe-8.0
    whispercpp:
      executable: C:/whisper.cpp/bin/whisper-cli.exe
      version: whisper.cpp-1.7.6
      model_storage_id: models-main
      model_relative_path: whisper/ggml-large-v3-turbo.bin
      profile:
        id: event-ko
        version: 1
        model_id: ggml-large-v3-turbo
        language: ko
        thread_count: 8
```

`storage_roots`의 key가 Worker가 접근 가능하다고 보고하는 Storage ID의 기준이다. root는 Worker 로컬의 절대경로지만 heartbeat·claim·결과·로그에는 보내지 않는다. Worker는 시작할 때 각 root를 `archive.StorageRoot`로 열고, 중복 ID·상대경로·열 수 없는 root가 하나라도 있으면 실행 loop를 시작하지 않는다. `CFG-008`

`process_logs`는 관리형 Worker boot에 필수인 ArchivePackage Storage와 분리된 로컬 증적 root다. 블록이 없거나 정책 값이 범위를 벗어나면 실행 loop를 시작하지 않는다. root는 시작 전에 존재해야 하며 Worker가 `StorageRoot`로 한 번 열어 실행 중 공유한다. `max_file_bytes`는 실행 하나, `tail_bytes`는 Control에 보낼 정제 tail, `retention`과 `max_total_bytes`는 로컬 보존 경계를 고정한다. 이 root와 raw 파일 경로는 heartbeat·결과·MCP·화면에 노출하지 않는다. Control에 연결하지 않는 단독 `archive-media` CLI에서는 이 블록을 생략할 수 있다. `CFG-007`, `WRK-019`

외부 도구 executable과 processor version도 Worker 로컬 strict 설정만 소유한다. FFmpeg·FFprobe 경로는 절대경로의 실제 파일이어야 하고 시작 전에 열 수 있어야 한다. Control의 StepExecutionSpec, heartbeat, 결과에는 executable 경로를 넣지 않는다. `CFG-009`

whisper.cpp는 선택 로컬 toolchain으로 둔다. 블록이 없으면 해당 Worker는 `TRANSCRIBE_AUDIO`를 등록하지 않는다. 블록이 있으면 executable·version·model Storage ID·model 상대경로·versioned transcription profile이 모두 있어야 하며, 모델은 boot 시 연 `StorageRoot`에서 실제 파일로 검증한다. 모델·실행 절대경로는 heartbeat·claim·결과에 넣지 않는다. `CFG-010`

Worker 추가·비활성화·처리 종류 변경은 Control의 로컬 설정 파일을 수정하고 서비스를 재시작해 적용한다. v1은 원격 enrollment 화면이나 API를 만들지 않는다. `CFG-001~003`, `SEC-004`

## 4. 연결과 상태

```text
Worker 시작
→ Control에 outbound connect + service credential
→ processing kind/tool version/storage/자원 snapshot 보고
→ 주기적 heartbeat
→ Step lease claim
→ progress/result 전송
```

heartbeat가 TTL을 넘으면 `OFFLINE`으로 간주하고 신규 lease를 주지 않는다. `WRK-002`

heartbeat의 자원 snapshot은 다음 값을 한 관측 시각으로 묶는다.

```text
snapshot_id, observed_at
cpu_average_5m, gpu_average_5m, idle_stable_for
available_memory_bytes, available_vram_bytes
scratch_free_bytes
heavy_running, light_running
busy_physical_device_ids
accessible_storage_ids, accepting_work
storage_observations: storage_id, connected, access kind, capacity/free, folder discovery state/revision
attached_volumes: physical_device_id, volume_id, source storage_id, model/label/filesystem, capacity/free
```

heartbeat는 64KiB 안의 고정 크기 집계만 보낸다. 폴더 수에 따라 커지는 `folder video count/input bytes` 목록은 heartbeat가 수락된 뒤 같은 `snapshot_id`의 별도 인증 inventory 요청으로 보낸다. Control은 정확히 현재 snapshot에만 그 목록을 결합하고 이 요청만으로 heartbeat TTL을 연장하지 않는다. 따라서 수백 개 폴더가 있는 드라이브도 Worker 연결 상태를 끊지 않는다. `WRK-006`

Windows Worker는 OS가 `DRIVE_FIXED`로 보더라도 USB·SD 같은 외부 data bus의 volume을 원본 후보로 관찰한다. 내부 시스템 volume과 1GiB 미만 보조 partition은 제외하고 serial·model·물리 용량으로 PhysicalDevice ID, volume GUID·filesystem으로 volume ID를 만든다. drive letter와 mount root는 identity에 넣지 않고 Worker process 안에만 둔다. 이 단계에서는 파일이나 폴더를 읽지 않는다. `DEV-007`

Control은 영속 원본 이력과 현재 연결을 같은 값으로 취급하지 않는다. 최신 유효 heartbeat의 attached volume에 stable volume·source Storage identity가 있으면 `VolumeConnectionState.CONNECTED`, Worker heartbeat는 유효하지만 해당 identity가 없으면 `DISCONNECTED`, Worker가 offline·disabled이거나 현재 관측 자체가 없으면 `UNKNOWN`이다. 연결 시각은 그 판정에 쓴 Worker snapshot의 관측 시각이다. 분리돼도 과거 ArchiveRun·ArchivePackage·Catalog 이력은 남지만 source Storage는 접근 불가가 되고 작업 관리의 새 ArchiveRun 원본 후보에서 제외된다. 다음 heartbeat에서 같은 identity가 다시 관찰되면 별도 화면 캐시 없이 `CONNECTED`로 돌아간다. `DEV-010`, `DSH-010`, `DSH-026`

Control은 attached volume 관찰을 identity만 가진 SOURCE_MEDIA 후보로 저장한다. 같은 논리 Volume이 다른 Worker에서 더 새로운 시각으로 관찰되면 물리 장치·Volume·Storage identity가 모두 같은 경우 현재 Worker를 새 관찰자로 바꾸고 candidate revision을 올린다. 과거 Worker의 늦은 관찰은 무시한다. 사람이 현재 candidate revision을 선택하면 불변 ArchiveIntakeSession을 저장하고, 다음 heartbeat 응답에 선택된 source storage ID를 같은 Worker에 반환한다. 새 작업용 선택 세션은 현재 candidate revision·Worker·source Storage가 모두 일치할 때만 투영하며, 과거 ArchiveRun의 불변 접수 세션은 실행 이력으로만 유지한다. Worker는 그때만 local mount를 `ReadOnlyStorageRoot`로 열어 폴더 inventory를 background에서 수행한다. 긴 탐색은 heartbeat·claim loop를 막지 않으며 최초 탐색은 `SCANNING`과 지금까지 찾은 폴더·영상 집계를, 성공하면 `READY`와 0이 아닌 revision을, root 개방이나 탐색 실패는 `FAILED`를 보낸다. `SCANNING`의 부분 목록은 화면 표시 전용이며 ArchiveRun 범위로 고정하지 않는다. 재탐색 중에는 이전에 완성된 `READY` 관측을 계속 보낸다. 보호·숨김 OS 하위 폴더는 건너뛴다. `DEV-008`, `DEV-010~011`, `ORG-001`

선택된 source 탐색 중 Volume이 분리되거나 선택에서 빠지면 Worker는 해당 scan context와 부분 관찰을 다음 heartbeat에서 취소·제거한다. 늦게 반환된 filesystem batch는 attempt identity가 맞지 않으면 버린다. Windows가 같은 drive letter를 다른 Volume에 다시 배정해도 새 stable volume·source Storage identity로 새 탐색을 시작하며 이전 탐색이 그 root를 계속 읽거나 결과를 덮지 않는다. 대용량 탐색의 부분 snapshot은 첫 결과 뒤 변경 디렉터리 수가 기하급수적으로 늘어나는 지점에만 전체 복사해 누적 할당을 제한하고, 완료 snapshot은 그대로 전부 보낸다. `DEV-014`, `DEV-011`

후속 경계에서는 새 원본 Volume의 연결을 확인한 즉시 사람의 작업 선택 전 읽기 전용 inventory를 시작한다. 검색 결과는 heap의 전체 목록이나 heartbeat payload가 아니라 연결 세션별 Worker 로컬 SQLite/WAL 인덱스에 제한된 batch로 기록하고, Control에는 요약·revision과 페이지 단위 상세만 투영한다. 검색이 끝난 revision만 새 작업의 READY 후보가 된다. 실제 Volume 분리를 운영체제 관찰로 확인하면 scan context·handle·memory·I/O 예약과 해당 세션의 DB·WAL·SHM 및 Control 상세 projection을 제거한다. 작업·ArchivePackage·Catalog·감사 이력은 인덱스 행을 참조하지 않고 생성 시 고정한 volume identity·revision·상대경로를 보존하며, 진행 중 작업은 `DEVICE`로 기다렸다가 재연결 후 새 revision을 검증한다. 이 경계는 현재 `DEV-008`의 선택 후 개방을 대체해야 하므로 구현 전에 그 의미와 테스트를 함께 개정하며, 지금은 `DEV-015~016` planned다.

Boot 설정 Storage와 선택된 attached source 모두 heartbeat에는 Storage ID와 집계만 보내며 파일 상대경로 목록과 root 절대경로는 보내지 않는다. 외장 Volume 관찰은 같은 root에 대해 SOURCE_MEDIA와 ARCHIVE_PRIMARY의 서로 다른 stable Storage ID를 함께 보고해 역할 capability를 섞지 않는다. Control은 등록 Storage topology, attached Volume·PhysicalDevice identity와 이 관측을 결합해 미분류 원본 폴더와 로컬·연결 네트워크·새 외장 드라이브 아카이브 후보를 투영한다. 접근 방식은 Worker Adapter가 `StorageAccessKind`로 보고하며 임의 Storage ID 문자열에서 추측하지 않는다. 같은 원본 Volume만 자체 아카이브 후보에서 제외하고 PhysicalDevice ID는 별도 I/O 충돌 키로 유지한다. `DEV-006~007`, `DEV-010`

Worker는 실행 중인 Step이 없을 때 현재 연결 외장 Volume의 source·archive Storage ID를 각각 `archive.StorageRoot` capability로 연다. 이 동작은 파일을 읽거나 쓰지 않으며, 실제 I/O는 fencing된 `StepExecutionSpec`이 해당 ID를 입력 또는 출력으로 지목할 때만 일어난다. 분리된 Volume의 동적 root만 닫고 boot 설정 root는 유지한다. 실행 중에는 handle을 닫지 않고 다음 유휴 heartbeat까지 조정을 미룬다. `DEV-013`, `WRK-010~012`

Dispatch의 Storage 허용 집합은 boot allowlist와 인증된 Worker가 같은 최신 heartbeat에서 보고한 attached Volume의 역할별 Storage ID를 합친 값이다. 따라서 임의 heartbeat Storage 주장은 계속 거부하지만 시작 시 알 수 없는 새 외장 Volume은 현재 OS 관찰과 접근 상태가 함께 있을 때만 lease 후보가 된다. `WRK-003`

Control은 Worker의 service credential과 단조 증가 snapshot ID를 검증해 마지막 snapshot만 저장한다. snapshot ID나 관측 시각이 역행한 heartbeat는 `STALE_RESOURCE_SNAPSHOT`으로 거부하고 마지막 정상 snapshot을 유지한다. heartbeat TTL을 넘긴 snapshot은 상태 조회에는 남아도 새 dispatch 판정에는 사용하지 않는다. credential·로컬 경로·process 원문은 snapshot에 넣지 않는다. `WRK-008`

`accepting_work`는 Worker가 종료·정리 중인지 나타내는 운영 gate이며 CPU·memory 기반의 최종 수용 판정이 아니다. Control은 heartbeat 원시값에 아직 해제되지 않은 ResourceReservation을 결합해 WorkerCapacity를 계산한다. 따라서 Worker가 보내는 `heavy_running`·`light_running`만으로 추가 작업 가능 여부를 표시하거나 queue admission을 결정하지 않는다. 실제 Step의 예상량과 예약 차감값을 사용한 Control의 판정이 실행·보류 상태의 기준이다. `DSP-004`, `DSH-014`

관리 Runner는 OS 자원 Probe 결과에 현재 claim한 Step의 `ResourceClass`를 합쳐 `heavy_running` 또는 `light_running`을 최소 1로 보고하고, 실행 정산 뒤에만 이 실행 표시를 비운다. 그러므로 WorkDiagnostic은 활성 예약과 Worker 실행 수를 같은 실행 경계로 비교할 수 있으며, 외부 subprocess가 별도 프로세스여도 실행 중인 작업을 0건으로 잘못 보고하지 않는다. `WRK-006`, `WRK-008`

배정된 Worker는 outbound claim으로 자신의 `RESERVED` Step 하나를 `RUNNING`으로 확정한다. claim과 결과 API는 매 요청의 Worker credential을 다시 검증하고, 결과는 현재 ExecutionLease owner와 fencing token이 일치할 때만 수락한다. Step 시작·성공·분류된 실패·취소 정산·lease 만료 복구의 결정적 상태 전이는 Core `WorkerLifecycleHandler`가 staged Job state와 Event로 만들며 SQLite Adapter는 Job state를 직접 바꾸지 않는다. staged change, 결과 receipt, Event·Command 기록, ResourceReservation·device reservation·lease 해제는 같은 SQLite transaction에서 정산한다. 응답을 받지 못해 같은 token과 같은 결과를 다시 보내면 저장된 receipt를 반환하고, 내용이 바뀌었거나 이미 다른 token으로 넘어간 결과는 `STALE_FENCING_TOKEN`으로 거부한다. `WRK-009`

### 4.1 실행 명세와 출력 기록

Worker가 Step 이름을 보고 입력 경로나 Profile을 추측하게 하지 않는다. Control은 배정 전에 versioned `StepExecutionSpec`을 Step에 확정하고 claim 응답에 spec digest와 함께 전달한다. 명세에는 다음 값만 허용한다. `WRK-010`

```text
spec_version, spec_digest
job_id, step_id, template_name, operation
input: entity ID, artifact ID, storage_id, normalized relative_path, checksum
output: expected entity ID, storage_id, normalized relative_path
profile reference와 operation별 typed parameter
```

절대경로, mount path, credential, executable 경로는 Control이 보내는 명세에 포함하지 않는다. Worker는 `storage_id`를 로컬 boot 설정의 `StorageRoot`에 대응시키고, 등록되지 않은 Storage·명세 digest 불일치·지원하지 않는 version 또는 operation을 실행 전에 거부한다.

성공 결과는 bool만 보내지 않고 immutable `StepOutputRecord`를 포함한다. 기록은 spec digest, 현재 fencing token, 생성한 entity·Artifact ID, Storage ID, 상대경로, checksum, 검증 증적 참조를 가진다. Control은 명세의 예상 출력과 일치하는 기록만 Step 완료·CommandRecord·Event·예약 해제와 같은 transaction에 저장한다. 동일 결과 재전송은 최초 기록을 바꾸지 않고, 오래된 token이나 명세 밖 출력은 아무 상태도 확정하지 않는다. `WRK-011`

선행 Step이 있는 실행 명세는 선행 출력이 확정되기 전에는 만들지 않는다. 성공 결과 정산 transaction이 최신 출력 기록과 Job의 고정 입력 문맥에서 다음 ready Step의 명세를 결정적으로 한 번 만들며, Worker가 보고한 절대경로나 임의 parameter를 후속 입력으로 승격하지 않는다. `APP-004`

운영 MediaOperation registry는 fixture mode 없이 typed operation을 등록 Adapter에 대응시킨다. 단독 `archive-media` CLI와 선택형 관리 runner가 같은 registry를 조립하며 하나의 명세를 정확히 하나의 Adapter에만 연결하고, 입력·임시 출력·게시 경로를 모두 시작 때 연 `StorageRoot`로 해석한다. 임시 출력 이름에는 실행 token을 포함하고 취소 정리는 그 token의 파일만 지운다. 같은 Storage 출력에는 교차 프로세스 배타 잠금을 먼저 획득해 CLI와 runner의 동시 게시를 막는다. ArchivePackage의 리비전·`current.json` 갱신도 StorageRoot의 package 단위 잠금으로 직렬화한다. operation이 없거나 둘 이상에 대응하면 subprocess를 시작하지 않는다. `WRK-012`, `APP-009~010`

실행 중인 Worker는 Control이 claim에서 내려준 주기로 현재 owner·fencing token의 lease heartbeat를 보낸다. 갱신은 token을 바꾸지 않고 같은 TTL만 연장하며, Worker는 서버와 로컬 시계 차이를 피하려고 절대 만료 시각이 아니라 상대 갱신 주기로 다음 시각을 계산한다. 결과 HTTP를 재전송하는 동안에도 갱신을 계속 시도한다. `WRK-013`

결과와 process 종료가 확인된 취소 정산 transaction은 읽기 projection 갱신·후속 Application observer·다음 dispatch 작업을 `worker_completion_outbox`에 함께 맡긴다. HTTP 요청은 이미 확정된 결과를 projection이나 dispatch의 일시 오류 때문에 거부하지 않고, 큰 검사 결과에서 Folder Job 생성을 끝까지 기다리지도 않는다. Worker는 응답을 받지 못한 결과·취소 보고의 동일 payload를 재전송하고 Control은 최초 receipt를 반환한다. Control의 조정 loop가 outbox를 재시작 뒤에도 읽어 세 후속 처리를 멱등 수행하고 전부 성공한 뒤에만 전달됨으로 표시한다. `WRK-014`, `CAN-005`

운영 executor는 checksum·명세 불일치를 영구 `INPUT`, 일반 subprocess·일시 I/O 실패를 `TRANSIENT`, 접근 불가 또는 용량 부족 Storage를 대상 ID가 있는 `DEVICE` 대기로 보고한다. `DISK`는 PhysicalDevice I/O 충돌 예약에만 쓴다. Storage 대기는 사람이 복구할 때까지 실행 시도 횟수를 소비하지 않는다. `WRK-015`

성공하지 못한 Worker 결과는 현재 오류 한 칸을 덮어쓰는 대신 재시도별 `StepFailureEvidence`를 Job 원장에 추가한다. 이 기록은 Worker·Step·typed operation·fencing token·실패 순서·workflow 시도·failure class·WaitReason과 대상 Storage ID·정규화 오류와 원인 code·정제 summary·process exit code·안전한 log reference·Control 관측 시각·결과 StepState를 가진다. 원문 stdout·stderr, 실행 인수, credential과 절대경로는 필드 자체를 두지 않는다. staged Job state, 같은 증적을 담은 Domain Event, Worker 결과 영수증, 예약·리스 해제는 한 transaction에서 확정하고 같은 결과 재전송은 최초 상태를 반환하므로 증적도 중복하지 않는다. 이후 성공해 현재 오류를 비워도 과거 실패 증적은 유지한다. `WRK-016`

외부 미디어 process는 Worker 로컬의 별도 `StorageRoot`에 실행 경계별 `ProcessLogArtifact`를 쓴다. 실행 중 출력은 같은 실행 ID의 `.partial` 파일에 단일 파일 상한까지만 쓰고, 실패 뒤 sync·hash 검증과 Storage 단위 quota lock을 거쳐 `.log`로 게시한다. 새 실행 전과 실패 게시 직전에 보존 기간과 총량을 넘는 오래된 완성 log부터 제거하므로 동시 실패가 총량 경계를 우회하지 않는다. 성공한 실행과 게시 실패의 임시 log는 제거하고 검증된 실패 실행만 raw 파일을 유지한다. Worker 결과에는 raw 파일 경로나 process 인수를 넣지 않고 Job·Step·operation·fencing token으로 계산한 ID, `evidence://process-log/<ID>` reference, digest, 기록량·전체량·잘림 상태와 비밀값·절대경로를 제거한 제한 tail을 보낸다. 로컬 게시를 완료하지 못해도 process 실행 결과는 잃지 않으며 가용 상태를 `CAPTURE_FAILED`로 보고한다. `WRK-019`

공통 실행기의 단계 진입은 측정된 처리율을 유지한다. FFmpeg가 내는 출력 millisecond는 시퀀스 결과의 시간축에 속한다. STT는 입력 순번·청크 순번·원본 범위와 whisper.cpp가 알린 숫자 진행만 전송한다. 전사 stdout은 로컬 process log에서도 제외하고 stderr 상세는 Worker 로컬에만 보관한다. Control은 이전 Worker의 전사 상세 tail도 숨긴다. Worker는 실제 stderr의 고정 형식과 요청한 청크의 출력 인수가 일치할 때만 JSON 쓰기 열기 실패를 기록한다. Windows에서는 요청한 JSON의 UTF-16 길이와 확장 경로 인수 사용 여부를 함께 기록한다. 출력 열기 실패 직후 StorageRoot에서 대상이 디렉터리로 확인되면 충돌 사실도 기록하고 기존 항목을 보존한다. 엔진이 모델 서명을 거부한 경우를 구분한다. 일치하는 관측이 없는 종료 코드는 일반 엔진 종료로 보존한다. 잘못된 WAV, 엔진 종료, JSON 누락·검증 실패, 게시 실패는 구체적인 원인 코드로 구분한다. Adapter는 JSON 디코딩 전에 원시 응답의 UTF-8을 검사한다. 손상 응답은 `WHISPER_OUTPUT_INVALID`로 기록하고 새 Transcript를 게시하지 않아 이전 패키지 리비전과 카탈로그가 유지된다. [진행 수집과 비용 기준](10-capabilities-observability.md#32-수집-비용과-관측-누락) · `TRN-008~009`, `OBS-011`

장시간 MediaOperation은 lease heartbeat와 별도로 제한된 빈도의 `StepProgressEvidence`를 제출한다. Worker가 현재 fencing token과 단조 증가 보고 순번으로 operation·계획/시작/완료/실패 상태·청크 순번과 전체 수·원본 시작/종료 millisecond·완료 비율을 보고하면 Control의 `WorkerLifecycleHandler`가 진행률·최근 관측 시각·변경 불가능한 증적과 Domain Event를 같은 Job transaction으로 확정한다. 같은 보고 재전송은 증적을 중복하지 않고 오래된 token은 거부한다. 이 구조에는 원문 stdout·stderr, 실행 인수, credential, 절대경로와 음성 내용이 없으며 작업 상세와 WorkDiagnostic이 같은 증적 ID를 읽는다. `WRK-017`

`RECONCILE_ARCHIVE_PUBLICATION`은 진단이 허용한 새 복구 Job에서만 쓰는 typed operation이다. 패키지 리비전 실패이면 현재 패키지와 동일 전사 JSON·시간 코드 text·자막 checksum을 확인해 이미 게시된 리비전을 재사용하거나 현재 리비전 위에 한 번만 추가하고, 카탈로그 실패이면 검증된 현재 패키지를 그대로 사용한다. 두 경우 모두 Storage 최상위 카탈로그를 원자적으로 다시 게시한다. 같은 명세가 다른 fencing token으로 다시 실행돼도 같은 전사 산출물로 패키지 리비전을 더 만들지 않는다. `WRK-018`

권장 상태:

```text
CONNECTING | ONLINE | BUSY | DRAINING | OFFLINE | DISABLED
```

`DRAINING`은 새 lease를 받지 않고 현재 작업만 끝내는 운영 상태다. Control 재시작 후 Worker는 다시 연결하고, 기존 Step은 lease 만료·idempotency 규칙으로 복구한다.

관리형 실행은 `archive-media runner`이며 strict boot YAML과 Credential Resolver에서 설정·credential을 각각 읽는다. Runner는 시작 직후와 설정 주기마다 Worker identity로 outbound heartbeat를 보내고 Control은 등록 identity·credential이 모두 일치할 때만 ONLINE/BUSY/DRAINING 상태를 갱신한다. 구현된 heartbeat는 단조 snapshot ID와 CPU·GPU·idle·memory·VRAM·scratch·slot·물리 장치·접근 Storage를 한 관측값으로 전송한다. Windows Probe는 시스템 memory·scratch·CPU를 직접 관측하고 NVIDIA 도구가 있을 때 GPU·VRAM을 추가한다. credential과 reference는 아카이브 작업 공간 응답에 포함하지 않는다. DPAPI process 경계는 `LRH-010`에서 검증하며, 실제 Windows Credential Manager Adapter의 서로 다른 서비스 계정 간 연결은 `WRK-007` acceptance로 별도 검증한다. `CFG-007`, `WRK-005~008`

## 5. Worker 선택

Dispatcher는 [05. 실행, 자원과 배포 설계](05-runtime-deployment.md)의 영속 queue에서 Step 하나를 고른 뒤 다음 순서로 Worker 후보를 거른다.

1. 설정에서 enabled이며 heartbeat가 유효함
2. Worker가 새 작업 수락 상태임
3. `RENDER`, `STT`, `DEVICE` 등 필요한 처리 종류 지원
4. 필요한 storage ID가 설정의 `allowed_storage_ids`와 heartbeat 접근 보고의 교집합에 있음
5. 특정 물리 장치 Step이면 해당 Worker에 pin하고 그 Worker가 offline일 때 다른 Worker로 우회하지 않음
6. TTL 안의 자원 snapshot으로 memory·scratch·device lock·해당 자원 분류 slot 조건 충족
7. heavy `BACKGROUND`이면 priority·idle hysteresis 조건 충족
8. 현재 실행 중인 같은 자원 분류 단계 수가 적은 Worker 우선, 동률이면 Worker ID 순

처리 종류와 storage 교집합 계약은 `WRK-003`, 물리 장치 pinning은 `WRK-004`가 검증한다. 자원 snapshot 기반 선택은 `DSP-002`, 원자 예약과 lease 발급은 `DSP-003`이 소유한다.

정적 조건을 만족하는 Worker가 하나도 없으면 StepState를 임의 WaitReason으로 바꾸지 않고 `QUEUED`를 유지하며 projection에 `NO_ELIGIBLE_WORKER` dispatch blocker를 기록한다. 정적 후보는 있지만 모든 후보가 자원 조건에서 거부되면 후보 정렬상 첫 Worker가 실제로 보고한 첫 WaitReason을 Step에 보존하고, 나머지 Worker별 blocker도 projection에 함께 둔다. snapshot이나 Worker 상태가 바뀌면 `WAITING`을 `QUEUED`로 깨운 뒤 원래 `ready_at`으로 다시 평가한다.

## 6. 저장 위치와 데이터 이동

- USB/SD/HDD가 Worker A에 연결되면 장치 분석·복사 단계는 Worker A에 고정한다.
- 원본 드라이브의 폴더 inventory는 사람 선택 뒤 그 SOURCE_MEDIA에 접근하는 Worker의 background read-only probe로 실행한다. 이후 FFprobe 검사는 같은 Worker의 typed operation으로 실행한다. Control이 로컬 경로를 다시 열거나 FFprobe를 직접 실행하지 않는다. `ORG-001`, `CFG-011`, `ORG-010`
- 로컬 scratch artifact는 다른 Worker가 직접 볼 수 있다고 가정하지 않는다.
- 다음 단계가 다른 Worker에서 실행되어야 하면 먼저 등록된 shared storage 또는 명시적인 transfer 단계로 publish한다.
- 네트워크 공유와 FTP·SFTP 아카이브 Storage도 Worker strict 설정과 전용 Adapter로 연다. storage ID가 같아도 각 Worker heartbeat가 실제 접근 가능 상태를 보고해야 하며 연결이 끊기면 관련 Step은 시도 횟수를 소비하지 않고 `WAITING/DEVICE`가 된다. `DEV-012`
- 네트워크 공유는 매핑 드라이브 문자를 업무 identity로 쓰지 않는다. UNC root·Secret Store credential reference·루트 식별 파일 방식은 `CFG-012`, FTP·SFTP endpoint·원격 root·credential reference·안전 게시 검증은 `CFG-013` acceptance 전까지 `planned`다. SMTP는 Storage Adapter가 아니라 전달 채널이다.
- SQLite 파일은 공유하지 않으며 모든 metadata write는 Control API를 통한다.

Archive Intake operation은 탐색·검사·렌더·QC·ArchivePackage 게시·오디오·STT·자막·패키지 리비전·ArchiveCatalog 게시로 나눈다. 모든 외부 도구는 executable과 argument 배열로 실행하고 StorageRoot 안 임시 출력 검증 뒤 게시한다. Windows 시퀀스 인수 검증은 Worker 재시작 후 루트 ArchiveCatalog의 패키지 1행과 video-info.csv의 원본별 경로·checksum을 확인한다. 녹화영상 인수 검증은 Control이 자동으로 별도 2단계 Command를 제출하는 경로를 사용한다. 같은 경로의 메타데이터와 CSV가 갱신되므로 1단계 게시 시점의 리비전 1을 확인하고, 2단계 완료 후 같은 패키지의 리비전 2와 전사·자막 경로를 대조한다. `APP-005`, `APP-007`

## 7. 장애와 중복 방지

- Worker 연결이 끊겨도 ExecutionLease TTL 전에는 같은 Step을 다른 Worker에 주지 않는다.
- TTL 뒤에는 현재 결과 receipt와 command idempotency receipt를 먼저 확인한다. 게시까지 끝났지만 결과 receipt 전 Worker가 중단된 상태를 실제 Artifact로 복구하는 `EXE-003` 수직 경로는 아직 planned이므로, 이 창에서는 자동 재실행을 안전하다고 완료 판정하지 않는다.
- 재발급한 ExecutionLease는 더 큰 fencing token을 가지며 이전 Worker의 늦은 결과는 거부한다. `WFL-007~009`
- 완료되지 않은 단계만 재대기한다.
- 임시 결과는 Worker별 scratch namespace에 둔다.
- 동일 물리 볼륨 lock은 Worker가 달라도 Control에서 전역 관리한다.

## 8. 아카이브 작업 공간의 Worker 상태 투영

아카이브 작업 공간의 Archive Workspace 영역은 Job 진행과 자원 병목을 판단할 수 있도록 Worker 상태를 투영한다.

- worker ID와 hostname
- ONLINE/BUSY/OFFLINE 상태
- CPU, memory, GPU/VRAM, scratch 여유
- 처리 종류와 tool version
- 접근 가능한 storage와 연결 장치
- 실행 중 Step, 마지막 heartbeat와 오류

연결 장치는 논리 Volume 작업 공간의 선택 상태와 PhysicalDevice I/O lock 충돌·안전 제거 가능 여부를 분리해 표시한다. 매체 소유권·반환 상태나 반환 승인 기능은 제공하지 않는다. Worker 등록·credential 변경·삭제 버튼도 제공하지 않으며 설정 파일을 바꾸고 Archive Control를 재시작한다. 전체 제품 표면은 [09. 아카이브 작업 공간 설계](09-archive-workspace.md)를 따른다.

### 원본 누락과 배정 차단의 판단 근거

원본 폴더 열람 실패는 탐색을 계속해 누락 수를 집계한 뒤 FAILED, revision 0, 첫 상대 폴더, 사유를 반환한다. 숨김·시스템 항목과 링크 등 기존 제외 정책은 누락 수에서 제외한다. 갱신 실패는 이전 READY를 폐기한다. 드라이브 후보 조회와 작업 공간이 같은 관측을 읽는다. `DEV-017`

Worker는 입력 읽기 실패 시 등록된 root의 접근성과 동일성을 재확인한다. root가 접근 가능한데 입력이 없거나 읽을 권한이 없으면 해당 입력 순번과 원인을 기록한다. root 접근 실패는 Storage ID와 DEVICE 대기로 보고한다. `WRK-015`

배정기는 후보별 거부 사유를 Job 리비전·Step·Worker·근거 snapshot·관측 시각과 함께 저장한다. 같은 리비전에서 거부 조건이 유지되면 샘플 번호만 바뀌어도 같은 관측을 유지한다. 진단은 현재 리비전에 해당하는 관측을 읽는다. 과거 판정은 보존하며 관측만으로 업무 리비전을 올리지 않는다. `DSP-005`
