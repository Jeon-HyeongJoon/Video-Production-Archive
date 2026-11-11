# 05. 실행, 자원과 배포 설계

## 1. Step 자원 분류

| 분류 | 예 | 기본 동시성 |
|---|---|---:|
| heavy | render, STT, 대용량 checksum/copy | Worker마다 1 |
| light | ffprobe, DB 기록, 작은 thumbnail | Worker마다 2 |
| control | HTTP, 승인, 상태 조회 | heavy slot 비사용 |

초기에는 render와 STT를 동시에 실행하지 않는다. 처리량 부족이 실제 queue 측정으로 확인되기 전에는 동시성을 높이지 않는다.

## 2. 영속 Dispatch Queue

Dispatch Queue는 별도 업무 원장이 아니라 저장된 Job·Step에서 다시 만들 수 있는 projection이다. 다음 조건을 모두 만족한 `QUEUED` Step만 dispatch 후보가 된다.

- 선언된 모든 선행 Step이 `COMPLETED`
- `next_eligible_at`이 없거나 현재 시각을 지남
- 유효한 ExecutionLease와 미정산 Cancellation이 없음
- Approval·입력·장치 조건 때문에 `WAITING`인 상태가 아님

후보는 `ready_at`, `priority`, `job_last_dispatch_sequence`, `ResourceClass`, 예상 memory·scratch, 필요한 ProcessingKind·Storage·PhysicalDevice를 가진다. `ready_at`은 선행 조건을 처음 만족한 시각이며 자원·우선순위 대기와 서비스 재시작으로 바뀌지 않는다.

선택 순서는 다음처럼 결정적으로 고정한다.

1. 명시적 `IMMEDIATE`와 `aging_after` 이상 기다린 `BACKGROUND`를 같은 높은 유효 우선순위로 둔다. 초기 `aging_after`는 30분이다.
2. 같은 Job에서는 가장 오래된 후보 하나만 현재 dispatch round의 head가 된다.
3. 높은 유효 우선순위부터, `job_last_dispatch_sequence`가 작은 Job, `ready_at`, `job_id`, `step_id` 순으로 선택한다.
4. lease를 실제로 확정한 뒤에만 Job의 dispatch sequence를 갱신한다. 충돌하거나 자원 예약에 실패한 시도는 순서를 소비하지 않는다.

이 규칙은 새 `IMMEDIATE`가 이미 실행 중인 lease를 선점한다는 뜻이 아니다. 다음 lease 선택에만 적용한다. 높은 우선순위 때문에 시작하지 못한 `BACKGROUND` Step은 `WAITING/PRIORITY`로 투영할 수 있지만 원래 `ready_at`을 보존하며, 높은 우선순위 집합이 사라지거나 aging 조건을 충족하면 다시 후보가 된다. 저장된 queue 필드와 dispatch sequence를 사용하므로 Control 재시작 전후의 다음 후보가 같아야 한다. `DSP-001`

## 3. Worker 판정과 자원 예약 순서

```text
1. enabled·heartbeat·ProcessingKind·Storage·pinning으로 Worker 후보 필터
2. heartbeat TTL 안의 자원 snapshot 확인
3. 예상 메모리 + 6GB 안전 여유
4. scratch 예상량 + 안전 여유
5. 물리 장치 lock
6. heavy/light semaphore
7. IMMEDIATE 우선 gate와 BACKGROUND idle hysteresis
8. 실행 가능한 후보 중 같은 ResourceClass 실행 수가 적은 Worker 선택
9. StepState.RESERVED + ResourceReservation + ExecutionLease 원자 확정
```

Resource Scheduler는 후보마다 memory → scratch → disk → slot → priority → idle 순으로 판정하며 첫 거부 이유를 그 Worker의 정확한 WaitReason으로 보존한다. 하나라도 실행 가능한 후보가 있으면 거부된 후보의 이유 때문에 Step 전체를 대기시키지 않는다. 실행 가능한 후보가 여러 개면 같은 자원 분류의 실행 수가 적은 Worker를 먼저 고르고 동률이면 Worker ID로 결정한다. 불확실한 예상치를 근거로 memory를 꽉 채우는 best-fit 최적화는 하지 않는다. `DSP-002`

ResourceReservation은 Worker·Step·resource snapshot ID, 예상 memory·scratch, slot 종류와 PhysicalDevice lock을 ExecutionLease에 연결한다. 새 snapshot에 이미 반영됐는지 확정할 수 없는 활성 예약은 모두 보고된 가용량에서 보수적으로 차감해 heartbeat 사이와 다음 heartbeat 뒤의 중복 예약을 막는다. 실행 시작을 확인한 예약을 snapshot 사용량과 조정하는 최적화는 Worker가 활성 lease ID를 보고할 때만 허용한다. Step을 `RESERVED`로 바꾸는 상태, 예약, lease와 fencing token은 같은 Command Store Unit of Work에서 전부 확정하거나 전부 폐기한다. 낙관적 리비전 충돌이 나면 새 snapshot과 queue 순서로 다시 평가한다. `DSP-003`

큐 admission도 같은 판정 하나를 사용한다. 새 Worker Step은 업무 기록 자체를 거부하지 않는다. 최신 snapshot에서 활성 예약을 차감한 뒤 실행 가능한 Worker가 있으면 바로 `RESERVED`로 확정하고, 정적으로 실행 가능한 모든 Worker가 자원 조건에서 막히면 시도 횟수를 소비하지 않은 `WAITING`과 첫 실제 WaitReason을 저장한다. `IDLE`·`MEMORY`·`SCRATCH`·`SLOT`·`DISK`·`PRIORITY` 대기는 다음 heartbeat 또는 예약 해제 때 같은 `ready_at`과 예상량으로 다시 평가하고, 조건이 회복되면 `WAITING`에서 직접 `RESERVED`로 전이한다. 한 후보의 보류가 같은 dispatch round의 다른 Job 후보 평가를 막지 않는다. Worker 미등록·처리 종류 불일치처럼 자원 WaitReason으로 표현할 수 없는 정적 부적합은 임의 이유로 추측하지 않는다. `DSP-004`

안전 조건은 `RES-003~007`로 검증한다. 예약 이후 실제 사용량이 예상치를 지속적으로 넘으면 Worker를 새 lease 제외 상태로 두고 경고 Event를 생성한다. 자원 snapshot 전송 계약은 `WRK-008`이 소유한다. WorkerCapacity는 snapshot 원시값과 활성 예약량, 예약 차감 뒤 가용량, heavy/light 사용·최대 slot 및 v1 기본 heavy/light Step 수용 결과를 같은 시점의 읽기 값으로 만든다. 화면용 기본 수용 결과는 운영자가 전체 여유를 빠르게 보는 기준이고 실제 queue admission은 반드시 대상 Step에 저장된 예상량으로 다시 계산한다.

## 4. 유휴 판정

최초 background 시작 조건:

```text
5분 평균 CPU < 35%
5분 평균 GPU < 20%
idle 조건 5분 유지
IMMEDIATE Step 없음
```

사용자 부하 후 재개는 10분 안정 상태를 요구한다. `RES-001~002`

IMMEDIATE Step은 idle 조건을 우회하지만 메모리, scratch, disk lock과 Worker heavy slot은 우회하지 않는다. light Step은 idle과 PRIORITY gate를 적용하지 않되 메모리와 light slot을 지킨다. `RES-003`, `RES-008`

## 5. 실행 중 부하 변화

1. 신규 BACKGROUND ExecutionLease 발급 중단
2. Worker/subprocess를 Below Normal priority로 유지
3. CPU thread 수 제한
4. 사용자 작업을 계속 방해하면 검증된 안전 지점에서 중단
5. 임시 출력과 resume 정보 기록 후 StepState.WAITING + WaitReason.IDLE로 반환

일반 FFmpeg 프로세스를 장시간 OS suspend하지 않는다. 재개가 필요한 작업은 segment 단위 출력과 최종 병합처럼 명시적인 checkpoint를 가진 profile로 만든다.

## 6. Subprocess 계약

모든 실행기는 다음 공통 인터페이스를 구현한다.

```text
Prepare(input IDs, profile) → immutable execution plan
Start(context, plan) → process handle
Observe(handle) → progress/log/metrics
Cancel(handle) → graceful stop, timeout 후 kill
Verify(temp outputs) → inspection/checksum/QC evidence
Publish(evidence) → artifact IDs
```

명령행은 배열 인수로 만들고 shell 문자열 결합을 사용하지 않는다. 출력은 임시 경로에서 검증한 뒤에만 `StorageRoot`의 동기화·검증·root-relative rename을 거쳐 게시한다. 로그에는 원문 credential이나 민감 경로를 노출하지 않는다. Core 계약은 `EXE-001~002`가 검증한다. 게시 뒤 Worker 결과 receipt 전에 중단된 실행을 실제 파일에서 조정하는 `EXE-003` Adapter는 아직 runtime에 연결되지 않아 planned다.

ffprobe Adapter는 Storage Adapter가 등록 root 안에서 해석한 실행 입력만 받아 `-show_format`, `-show_streams` JSON을 요청하고 이를 정규화한 `MediaInspection`으로 변환한다. CI에서는 격리 실행 파일로 인수 배열과 변환을 검증한다. 전용 Windows acceptance는 실제 H.264/AAC 입력을 만들고 운영 ffprobe가 container·길이·크기·bitrate·stream을 기록하는지와 입력 checksum 불변을 검증한다. `MED-002~003`

Windows FFmpeg acceptance는 실제 출력에 실제 ffprobe 검증을 적용하고 게시 checksum, Profile, Worker, thread 상한, 실행시간, peak working set과 host를 테스트 증적에 기록한다. 렌더는 H.264/AAC MP4와 1920×1080 경계를, 음성 추출은 mono·16kHz·PCM WAV를 확인한다. 재실행 절차는 `deploy/windows/run-media-acceptance.ps1`이 소유한다. `RND-003`, `AUD-003`

Windows Archivist acceptance는 DPAPI secret의 새 프로세스 복호화와 실제 Archivist Host·계약 CLI fixture의 강제 재시작을 한 Windows 장비에서 실행한다. 재실행 절차와 정규화된 테스트 증적은 `deploy/windows/run-archivist-acceptance.ps1`이 소유한다. 실제 Codex 또는 Claude 계정 acceptance와 fixture acceptance는 같은 결과로 간주하지 않는다. `LRH-009~011`

구독 로그인 Codex는 Windows Service·SYSTEM·SSH batch logon에서 시작하지 않는다. 배포본은 같은 버전의 `codex.exe`, `codex-code-mode-host.exe`, `codex-exec-server.exe`를 고정 runtime root의 한 디렉터리에 둔다. `deploy/windows/configure-interactive-archivist.ps1`이 이 실행 번들을 확인하고 현재 console 사용자의 대화형 예약 작업에서 `codex login status`를 먼저 실행하며, 성공한 같은 SID와 0이 아닌 Session에서만 Archivist Host를 시작한다. Control의 Turn은 이 Host에 전달되고 Host가 구조화 stdio로 `codex app-server`를 새로 열므로 Windows Terminal 화면이나 ANSI 출력을 중계하지 않는다. 사용자가 로그아웃하면 Host는 offline이 되고 Worker 작업은 계속되며 다음 로그인 뒤 예약 작업이 Host를 다시 시작한다. `LRH-020`

## 7. Windows 서비스와 사용자 Tray 배포

- `archive-control`, 선택형 `archive-media runner`, 필요한 PC의 `archivist-host`를 별도 Windows Service로 등록
- 자동 시작은 delayed start 사용
- 서비스 계정에 필요한 storage root와 Secret Store 권한만 부여
- 화면 잠금과 무관하게 서비스 지속
- AC 전원 시 sleep/hibernate 비활성화
- 정전 후 자동 부팅과 UPS 종료 hook 구성
- 로그와 DB는 scratch 및 외장 archive와 분리

`archive-tray.exe`는 위 서비스와 다른 사용자 Session Adapter다. 트레이 아이콘은 Session 0에 표시할 수 없으므로 Windows Service로 등록하지 않는다. 설치기는 현재 console 사용자의 `AtLogOn`·`Interactive`·`Limited` 예약 작업 `Archive Tray`를 등록하고, 표준 runtime root의 실행 파일에 검증된 Chrome 절대경로와 고정 아카이브 작업 공간 URL을 인수로 준다. 왼쪽 클릭은 Chrome에 `--new-tab <url>` 인수 배열을 한 번 제출한다. Tray 종료나 로그아웃은 Control·Worker·Archivist Host를 건드리지 않으며, Control·Worker의 부팅 자동 시작도 로그인과 무관하게 유지된다. `DSH-041`, `CFG-016`

Windows v1 설치는 `C:\Archive` 하나를 runtime root로 사용한다. 선택한 ARCHIVE_PRIMARY가 이 root라는 뜻은 아니며, 프로그램 작업 공간과 기록물 Storage를 섞지 않는다.

```text
C:\Archive\
├── runtime\
│   ├── current\          # 서비스가 실행하는 검증된 바이너리
│   └── releases\<rev>\   # 되돌릴 수 있는 release
├── config\               # YAML과 Secret Store reference만 보관
├── data\                 # SQLite와 migration 상태
├── logs\                 # 서비스별 회전 로그
├── scratch\              # 재생성 가능한 임시 출력
├── evidence\             # acceptance·golden·배포 증적
└── legacy\               # 이전 위치 inventory와 복구 가능한 격리
```

`runtime\current`에는 서비스 실행 파일과 함께 `archive-tray.exe`를 둔다. 사용자에게 보이는 Tray는 PC 전원 직후가 아니라 Windows 로그인 직후 시작된다. 대화형 desktop이 없는 로그인 전에는 아이콘을 표시할 수 없다는 운영체제 경계를 자동 시작 실패로 해석하지 않는다.

새 release는 별도 `releases/<rev>`에 복사·검증한 뒤 `current`를 전환한다. `C:\Video-Production-Archive`, 과거 `deploy/<rev>`와 다른 임시 배포는 먼저 inventory에 기록하고 서비스·설정·DB·패키지 checksum을 대조한 뒤 `legacy`로 옮긴다. 자동 삭제는 하지 않으며 ArchivePackage나 catalog는 선택한 아카이브 Storage에 그대로 둔다. Windows acceptance는 실행 중인 프로세스와 예약 작업이 `runtime/current`만 가리키는지, 역할별 설정 경로와 DB·scratch가 분리됐는지, 이전 경로와 패키지 검증 기록이 복구 가능한 `legacy`에 남았는지를 함께 확인한다. `CFG-014`

`deploy/windows/manage-runtime-layout.ps1`은 기본 `Plan`, 비파괴 `Inventory`, 해시를 검증하는 `Stage`만 제공한다. 서비스의 `current` 전환과 이전 경로 격리는 Windows acceptance에서 설정·DB·패키지 manifest를 대조한 뒤 수행한다.

```powershell
.\deploy\windows\manage-runtime-layout.ps1 -Mode Plan
.\deploy\windows\manage-runtime-layout.ps1 -Mode Inventory
.\deploy\windows\manage-runtime-layout.ps1 -Mode Stage -Revision <git-revision> -SourceDirectory <binary-directory>
```

Windows release 묶음은 `make build-windows`로 만들며 Tray만 Windows GUI subsystem으로 링크해 console 창을 만들지 않는다.

실제 설치를 전환하고 이전 경로를 격리한 뒤 다음 acceptance를 실행한다. 증적 디렉터리는 전환 전후 API·예약 작업·프로세스·DB migration·패키지 manifest 검증과 이동 목록을 포함해야 한다.

```powershell
$env:ARCHIVE_WINDOWS_RUNTIME_LAYOUT_ACCEPTANCE = '1'
$env:ARCHIVE_WINDOWS_RUNTIME_ROOT = 'C:\Archive'
$env:ARCHIVE_WINDOWS_RUNTIME_EVIDENCE = 'C:\Archive\evidence\<run-id>'
go test ./internal/application/controlruntime -run TestWindowsRuntimeUsesSingleRootAndRetainsLegacyEvidence -v
```

네트워크 설정은 서비스 설정과 Windows Firewall에 이중으로 둔다.

```text
listen_address: 사무실 NIC의 고정 내부 IP
allowed_cidrs: 실제 사무실 subnet만 지정
public_profile_inbound: deny
port_forwarding/upnp: disabled
mcp_listen_address: 127.0.0.1  # auth disabled인 경우
```

Tailscale 같은 overlay network를 사용할 때도 전체 CGNAT 대역을 허용하기보다 해당 장치/사용자 ACL과 필요한 주소만 허용한다.

단독 작업 PC에는 `archive-media`만 설치하며 CLI 실행은 Control 연결이나 service credential을 요구하지 않는다. 관리형 원격 PC에는 같은 실행 파일의 `runner` 하위 명령을 서비스로 등록한다. runner가 Control로 outbound 연결하므로 inbound 포트를 열지 않으며, Worker ID와 credential reference는 양쪽 로컬 설정에 사전 등록한다. 아카이브 작업 공간의 사용자 인증 비활성 설정은 runner service 인증을 끄지 않는다. `APP-009~010`

로컬 CLI가 필요한 PC에는 Archivist Host를 추가한다. Archivist Host도 Control로 outbound 연결하며 CLI용 MCP proxy는 loopback에서만 연다. 허용 backend·CLI 실행 파일·workspace root와 credential reference를 양쪽 시작 설정에 사전 등록하고, raw credential·임의 path·shell command를 원격 입력으로 받지 않는다. 전체 Orca 앱 설치는 필수가 아니며 Embedded Core backend를 기본으로 한다. Orca CLI backend를 선택한 배포만 별도 Orca 설치를 요구한다. `LRH-001`, `LRH-007`, `MCP-001`

구독 로그인 Codex CLI를 등록한 Windows PC에서는 Codex standalone release의 `bin`에 있는 세 실행 파일을 `C:\Archive\tools\codex\<version>`에 함께 배치한 뒤 관리자 터미널에서 다음을 한 번 실행한다. 설치기는 전체 실행 번들을 확인하고 기존 작업 정의를 증적 폴더에 백업하며 legacy 중복 Host 작업을 비활성화한 뒤 현재 로그인 사용자의 `InteractiveToken` 작업으로 canonical Host를 등록한다. `Probe`는 작업을 바꾸지 않고 실행 번들과 같은 사용자 문맥의 Codex 로그인만 확인한다.

```powershell
.\deploy\windows\configure-interactive-archivist.ps1 -Mode Probe -Root C:\Archive
.\deploy\windows\configure-interactive-archivist.ps1 -Mode Install -Root C:\Archive
```

같은 PC에서 Tray를 설치·확인하거나 제거할 때는 다음 명령을 사용한다. `Probe`는 파일·URL·Chrome·로그인 사용자를 읽기 전용으로 확인하고, `Install`은 canonical 예약 작업을 교체한 뒤 현재 사용자 Session에서 시작하며, `Remove`는 Tray 작업과 Tray process만 제거한다.

```powershell
.\deploy\windows\configure-tray.ps1 -Mode Probe -Root C:\Archive -DashboardURL https://<archive-control>/
.\deploy\windows\configure-tray.ps1 -Mode Install -Root C:\Archive -DashboardURL https://<archive-control>/
.\deploy\windows\configure-tray.ps1 -Mode Remove -Root C:\Archive
```

## 8. 종료와 재시작

Control 종료:

1. 새 Command/lease 수락 중단
2. Notification feed에 shutdown 예정 알림
3. DB transaction 완료
4. Worker lease는 만료 복구 가능 상태로 둠

Worker 종료:

1. 새 lease claim 중단
2. 안전 종료 가능한 subprocess에 취소 전달
3. 마지막 heartbeat와 중단 이유 보고
4. timeout 후 프로세스 트리 종료

Archivist Host 종료:

1. 새 ArchivistTurn lease claim 중단
2. 마지막 runtime event cursor와 resume reference 확정
3. 진행 중 turn의 lease를 반납하거나 만료 복구 가능 상태로 둠
4. CLI에 graceful stop을 전달하고 timeout 뒤 process tree 종료

재시작 시 Worker에는 `WFL-004`, `WFL-008~009`, `WRK-013~014`의 ExecutionLease 갱신·결과 receipt·completion outbox 복구 규칙을 적용한다. 게시 뒤 결과 receipt 전 중단 복구는 `EXE-003` planned 경계로 명시한다. Archivist Host에는 `LRH-003~006`, `LRH-008`의 ArchivistTurnLease, durable session 재해석과 cursor replay 규칙을 적용한다.

## 9. 관측성

- 구조화 로그: job_id, step_id, command_id, worker_id, archivist_host_id, archivist_session_id, archivist_turn_id, request_id
- metric: queue depth, wait reason, CPU/GPU/memory, disk throughput, failure rate
- Event: 상태 전이, 승인, fallback, device connect/disconnect
- Notification: Event 또는 metric에서 만든 실시간 메시지. Event 기반 알림만 재연결 replay
- process 원문 로그: 회전 파일, DB에는 위치와 tail만 저장

로컬 CLI ArchivistTurnEntry는 `LRH-013`의 허용 목록으로 정제한다. 외부 Provider 관련 필드의 `AGT-008` 정책은 Provider Router가 runtime에 연결될 때까지 planned다. feed의 replay 분리는 `NTF-001`, 실제 HTTP/SSE 재연결은 `NTF-002` integration test가 검증한다.

목표 runtime에서는 Capability Executor의 공통 Evidence Sink가 시작·진행·완료·실패와 상관 ID를 기록한다. 현재 requirement test는 CI evidence runner가 구조화한 뒤 CI artifact로 누적한다. 로컬 테스트 실행은 완료 판정 증적으로 사용하지 않는다. 운영 증적과 테스트 증적은 형식을 공유할 수 있지만 저장 수명주기와 원장은 분리한다. 모든 운영 Capability를 감싸는 Evidence Sink는 아직 planned인 `OBS-001`, 테스트 증적은 구현된 `TST-001`이 소유한다. 자세한 계약은 [10. 도메인 기능 실행과 검증 증적](10-capabilities-observability.md)을 따른다.
