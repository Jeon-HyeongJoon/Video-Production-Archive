# 01. 시스템 컨텍스트

## 1. 사용자와 외부 시스템

| 대상 | 책임 | 신뢰 경계 |
|---|---|---|
| 담당자 | 입고, 메타데이터 입력, 검수, 즉시 실행 | 허용된 내부망에서 접근하는 v1 무로그인 사용자 |
| 로컬 설정 권한 보유자 | Provider·Worker·Archivist Host·Storage와 보안 정책의 시작 설정 | OS ACL로 보호된 로컬 파일과 서비스 재시작, 아카이브 작업 공간 Principal이 아님 |
| 외부 Archivist | 조사, 계획, 오류 설명, 허용된 도구 호출 | 신뢰하지 않는 외부 실행자 |
| AI Provider | 채팅 응답과 콘텐츠 해석 | 사외 데이터 전송 경계 |
| 카메라/SD/USB/HDD | 원본 입력과 아카이브 출력 | 식별·상태가 변할 수 있는 장치 |
| FFmpeg/ffprobe/STT | 미디어 처리 subprocess | 실패·hang·부분 출력 가능 |
| Worker PC | 렌더·STT·입고를 분산 실행 | 별도 service identity를 가진 실행 노드 |
| 로컬 CLI PC | Codex·Claude CLI 세션과 도구 연결을 지속 | Archivist Host service identity와 고정 workspace 경계 |

아카이브 작업 공간과 API는 공용 인터넷에 공개하지 않는다. Windows Firewall과 애플리케이션 CIDR allowlist를 모두 통과한 사무실 내부 주소만 허용한다. 내부 IP는 장치 위치를 제한할 뿐 사용자 신원을 증명하지 않으므로 인증 비활성 모드에서는 모든 요청을 익명 담당자로 취급한다.

## 2. 핵심 사용자 흐름

### 행사 전체 녹화

원본 입고 → 기술 분석 → proxy/thumbnail/audio → STT → 요약 → 통합 QC → 사람 승인 → 아카이브

### VJ 소스

클립 입고 → OK 컷 → Timeline/marker → preview → 사람 승인 → render → QC → 아카이브

### 최종 편집본

입고/중복 검사 → 버전 기록 → 분석 → proxy/대표 이미지 → QC → 납품·보관 위치 기록

### 대용량 원본 파일 입고

작업 공간 드라이브 선택 → 원본 자산 등록 → 첫 사본 복사·검증 → 두 번째 사본 복사·검증 → 작업 공간 드라이브 안전 해제

### 아카이브 작업 공간에서 사람과 로컬 CLI Archivist 협업

```text
사람 직접 경로: 버튼·폼·API → Command/Job 실행
Archivist 경로: Host·CLI 선택 → ArchivistSession → ArchivistTurn → MCP 조회·허용 Command 또는 ActionCard
공통 결과: Command·Event·증적 → 같은 작업 상세 → 사람과 다음 ArchivistTurn이 이어서 처리
```

사람의 직접 경로는 ArchivistSession이나 AI 가용성을 요구하지 않는다. 인증된 MCP Capability의 Archivist 상태 변경은 저장된 `MANUAL` 제어 모드에서 거부한다. `ASSISTED` 또는 `GATED`·`DESTRUCTIVE` 명령은 ActionCard로 제안하고, 저장된 `ARCHIVIST` 제어 모드의 비파괴 `PROCESS`만 직접 실행할 수 있다.

## 3. 운영 불변 조건

- 원본은 입고 후 내용이 바뀌지 않는다. `ARC-001`
- 상태 변경은 공통 Command 계약을 통과한다. `CMD-001~008`, `PER-001`
- 파괴적 작업은 Archivist가 승인할 수 없다. `SEC-001`
- 외부 Provider Account Router를 연결한 후에도 모든 AI 계정이 실패하면 수동 도메인 기능은 계속되어야 한다. 이 계약은 `AGT-007` planned다. v1 로컬 CLI Archivist가 unavailable이어도 사람의 직접 경로는 계속된다.
- PC 사용 부하가 있는 동안 background heavy 단계는 새로 시작하지 않는다. `RES-001~003`
- 대용량 파일 입고는 두 입고 사본 검증 뒤 작업 공간 드라이브를 해제하며 매체 소유권·반환 상태나 반환 승인을 만들지 않는다. `PIP-004`
- 아카이브 작업 공간/API는 명시한 내부 CIDR에서만 접근할 수 있다. `NET-001`
- 인증을 끄면 사용자별 계정·작업 소유자를 만들지 않고 모든 UI 요청을 `internal-operator`로 처리한다. `NET-002`
- 무로그인 상태에서도 브라우저 mutation은 Origin과 CSRF 검사를 통과해야 한다. `NET-006`
- v1은 원격 관리 화면/API를 등록하지 않고 OS ACL로 보호된 로컬 시작 설정만 사용한다. `CFG-001~002`, `SEC-004`
- Worker PC는 UI 로그인과 별개로 사전 등록과 개별 service credential을 요구한다. `WRK-001`
- Archivist Host도 UI 로그인과 별개로 사전 등록과 개별 service credential을 요구하고 Control로 outbound 연결한다. `LRH-001`
- ArchivistSession은 선택한 Host에 고정하며 offline을 이유로 다른 PC로 자동 이동하지 않는다. `LRH-002`
- 채팅 문구는 Archivist 지시가 될 수 있지만 Approval은 아니며, 승인 필요한 명령은 ActionCard의 명시적 조작만 Approval 또는 Command 후보를 만든다. `DSH-003`
- 사람의 직접 Command와 Archivist의 허용된 Tool Call은 같은 Command Bus와 업무 원장을 사용한다. `COL-001~002`

## 4. 시스템 밖의 책임

- 카메라 자체의 녹화 무결성
- 제공자 청구 시스템의 최종 금액 계산
- RAID를 백업으로 간주하는 운영
- 개인 구독 계정 순환을 통한 API 제한 우회
- 편집자의 창작 판단을 완전 자동화하는 기능
- 유효한 승인과 대상 재확인 없는 원본 삭제 또는 디스크 포맷
- 외부 매체의 소유권, 대여, 반환과 인수인계 이력 관리
- 아카이브 작업 공간에서 raw terminal 원격 조종, 임의 shell 실행 또는 Orca 개발 workspace 관리

## 5. 서비스 수준 목표

초기 목표는 처리 완료 시간이 아니라 제어 기능의 생존성이다.

| 항목 | 초기 목표 |
|---|---|
| Control API | heavy 단계 실행 중에도 상태 조회와 취소 요청 가능 |
| 데이터 안전 | 성공 표시 전에 출력 검증과 영속 이벤트 기록 |
| 장애 복구 | 만료 lease를 감지해 중복 없이 재대기 |
| CLI 세션 복구 | Host·CLI 재시작 뒤 완료 Tool Call과 Command를 반복하지 않고 같은 Session 재개 |
| 수동 폴백 | AI 불가 상태에서도 표준 Workflow 생성·실행 가능 |
| 자원 보호 | 작업 예약 후 6GB 메모리 여유 유지 |

정량 latency와 처리량은 실제 PC/코덱/외장 디스크 benchmark 후 별도 성능 요구사항으로 승격한다. 측정 전 수치를 보장값으로 문서화하지 않는다.
