# 12. 취소와 보상 설계 제안

> 상태: 단계적 v1 결정. `RESERVED` 취소와 실행 중 Worker stop·정리 확인 정산은 `CAN-001~004`로 구현했고, 불명확한 외부 효과 조정과 보상 항목은 아래 분할안에 따라 순차 구현한다.

## 1. 문제를 세 가지로 분리한다

| 개념 | 의미 | 기존 기록 처리 |
|---|---|---|
| 취소(Cancellation) | 아직 끝나지 않은 실행과 후속 실행을 멈춘다 | 완료된 Command·Event·Artifact는 유지 |
| transaction rollback | DB transaction 실패를 원자적으로 폐기한다 | 어떤 업무 기록도 확정하지 않음 |
| 보상(Compensation) | 이미 확정된 효과를 새 업무 행동으로 상쇄한다 | 기존 기록을 고치지 않고 새 Command·Event 추가 |

제품 화면과 API에서 `rollback`을 범용 되돌리기 이름으로 사용하지 않는다. 사용자가 “취소”를 눌렀다고 이미 게시된 파일이나 완료된 Step이 과거에 없었던 일이 되지는 않는다.

DB transaction rollback의 원자성은 DB 기록에만 적용한다. 외부 process나 파일 publish가 먼저 일어났다면 transaction 실패만으로 그 효과가 사라졌다고 간주하지 않고 기존 publish 조정 계약으로 확인한다.

## 2. 핵심 원칙

1. 취소는 미래 지향적이다. 실행 중인 process와 아직 시작하지 않은 후속 Step만 멈춘다.
2. `COMPLETED`, `FAILED`, `CANCELLED` Step은 불변으로 유지한다.
3. 완료된 효과를 바꿔야 하면 원래 Command·Event·Artifact를 참조하는 새 보상 Command를 만든다.
4. 취소 요청 수락과 실제 process 종료를 같은 시점으로 표시하지 않는다.
5. 취소와 완료가 경합하면 DB에 먼저 확정된 명령이 우선한다.
6. 외부 효과가 발생했는지 알 수 없으면 성공이나 취소로 추정하지 않고 조정 대상으로 남긴다.
7. ArchivistTurn 취소는 Archivist가 이미 확정한 Tool Call과 Command를 되돌리지 않는다.

## 3. 취소 대상별 동작

| 대상 상태 | 취소 결과 |
|---|---|
| `QUEUED`, `WAITING` | 즉시 `CANCELLED`. 외부 실행 없음 |
| `RESERVED` | 새 실행을 fence하고 자원 예약을 해제한 뒤 `CANCELLED` |
| `RUNNING`, `VERIFYING`, `PAUSED` | 취소 요청을 먼저 기록하고 Worker에 graceful stop 전달. 종료·임시 출력 정리 확인 뒤 `CANCELLED` |
| `COMPLETED`, `FAILED` | `ALREADY_TERMINAL`로 취소 거부. 가능하면 보상 계획 생성 |
| `CANCELLED` | 같은 멱등 key는 최초 취소 receipt를 replay하고 새 요청은 `ALREADY_CANCELLED` 반환 |

Job 취소는 Job 자체를 지우지 않는다. 기본 범위는 `JOB_REMAINDER`로 두고, 실행 중 현재 Step과 아직 시작하지 않은 후속 Step을 한 번의 영향 미리보기에서 확인한다.

- 시작하지 않은 현재·후속 Step은 취소한다.
- 실행 중 Step은 중지 절차를 거친다.
- 이미 완료된 Step과 게시 Artifact는 그대로 남긴다.
- 일부 Step이 완료됐다면 화면에 `일부 결과 남음`을 별도 취소 결과로 표시한다.

ArchivistTurn 취소는 현재 답변 생성을 중지한다. 그 Turn에서 이미 완료된 ArchivistToolCall receipt와 Command는 유지하며, 사용자가 효과까지 되돌리려면 별도 보상 ActionCard를 선택한다.

## 4. 취소 요청과 대상 상태를 분리한다

StepState에 `CANCELLING`을 추가하지 않는 안을 권장한다. 대신 취소 요청과 진행을 별도 투영으로 둔다.

```text
CancellationRequested
→ StopDispatched
→ TargetStopped | StopFailed | ExternalEffectUncertain
→ StepCancelled | CompensationRequired
```

취소 기록은 다음을 가진다.

```text
cancellation_id
target_type, target_id, expected_revision
scope: CURRENT_STEP | JOB_REMAINDER | ARCHIVIST_TURN
actor, reason, idempotency_key
requested_at, settled_at
outcome: STOPPED | PARTIAL | REFUSED | UNCERTAIN
remaining_effect_references[]
```

이렇게 하면 Step은 실제 실행이 끝나기 전까지 기존 상태를 유지하면서도 아카이브 작업 공간에서는 `취소 요청`, `중지 중`, `일부 결과 남음`을 정확히 보여줄 수 있다.

## 5. Worker와 Archivist Host 중지 계약

```text
Control이 취소 Command 확정
→ 같은 transaction에서 현재 lease를 취소 fence로 폐기
→ Worker/Archivist Host에 graceful stop
→ 제한 시간 안에 종료되지 않으면 kill
→ 임시 출력·열린 handle·게시 receipt 조사
→ 정리 결과 보고
→ Control이 CANCELLED 또는 PARTIAL 확정
```

- 취소 fence 이후 이전 fencing token의 성공 보고는 받지 않는다. 취소 요청 기록과 lease 폐기는 같은 Unit of Work에서 확정한다.
- stop/kill 성공만으로 출력이 없다고 가정하지 않는다.
- publish receipt나 실제 게시 파일이 발견되면 기존 재시작 조정과 같은 방식으로 효과를 등록하고 `PARTIAL`로 끝낸다.
- 외부 process가 끝났는지 확인할 수 없으면 `UNCERTAIN`을 유지하고 자동 재실행하지 않는다.

### Queue와 ResourceReservation 처리

취소 Command가 확정된 대상은 같은 transaction부터 dispatch 후보에서 제외한다. 자원은 상태에 따라 다음 시점에 해제한다.

| 상태 | queue·예약 처리 |
|---|---|
| `QUEUED` | queue projection에서 제외. 자원 예약 없음 |
| `WAITING` | wake 조건이 충족돼도 다시 `QUEUED`로 올리지 않음 |
| `RESERVED` | lease를 fence하고 아직 process가 시작하지 않았음을 확인한 같은 Unit of Work에서 ResourceReservation 해제 |
| `RUNNING`, `VERIFYING`, `PAUSED` | lease를 fence하되 slot·memory·scratch·PhysicalDevice 예약은 stop과 정리 확인 전까지 유지 |
| `UNCERTAIN` | Worker process와 외부 효과를 조정할 때까지 관련 Worker·PhysicalDevice를 새 dispatch에서 격리 |

취소 직후 예약을 먼저 풀면 이전 process와 새 process가 같은 scratch나 PhysicalDevice를 동시에 사용할 수 있으므로 금지한다. stop·kill 뒤 process 부재, 열린 handle 정리와 publish receipt 확인이 모두 끝난 시점에만 예약을 해제한다. 일반 Worker slot은 process 부재가 확인되면 해제할 수 있지만 효과가 불명확한 PhysicalDevice와 staging namespace는 조정 완료까지 격리한다. 보상 Command로 만들어진 Job·Step은 특별 우선 경로를 사용하지 않고 일반 Dispatch Queue, ResourceReservation과 Approval 정책을 다시 통과한다.

## 6. 취소와 완료 경합

### 완료가 먼저 확정된 경우

취소 Command를 `ALREADY_TERMINAL`로 거부한다. 완료된 결과를 숨기지 않고 보상 가능 여부를 제시한다.

### 취소가 먼저 확정된 경우

기존 lease를 fence하고 늦은 완료 보고를 거부한다. 다만 외부 파일이나 장치 효과가 실제로 생겼다면 무시하지 않고 조정 후 `PARTIAL` 또는 `COMPENSATION_REQUIRED`로 표시한다.

### 결과가 불명확한 경우

같은 idempotency key로 취소 요청을 재전송해 최초 receipt를 조회한다. 상태 부재만으로 취소나 완료를 재시도하지 않는다.

## 7. 보상은 범용 Undo가 아니다

각 Command 종류가 자신에게 맞는 보상 가능성을 선언한다.

| 원래 효과 | 보상 예시 |
|---|---|
| Job 생성·실행 | 남은 Step 취소. Job과 완료 이력은 보존 |
| metadata 리비전 | 이전 값을 복사한 새 리비전 생성 |
| 게시된 Rendition·Summary | 기존 Artifact를 지우지 않고 새 결과가 대체하도록 관계 기록 |
| 잘못된 Approval | 같은 대상 리비전에 더 큰 Event 순번의 `REJECTED` 기록 |
| 아카이브 확정 | 기존 패키지를 고치지 않고 새 패키지가 대체 |
| 납품 기록 | 삭제 대신 정정 기록 추가 |
| 장치 포맷·이미 외부 전달된 파일 | 되돌릴 수 없음. 사전 Approval·Confirmation으로만 방지 |

보상 계획은 원래 `command_id`, 결과 Event와 효과 참조, 실행할 보상 Command 목록, 실행 가능 여부와 필요한 Approval을 가진다. 보상도 일반 Command Bus, expected revision, 멱등 key와 권한 정책을 통과한다.

## 8. 아카이브 작업 공간 표현

취소 버튼을 누르기 전에 다음 영향을 보여준다.

```text
중지 대상       실행 중 Step 1
취소 대상       대기 Step 4
유지 결과       완료 Step 3, Artifact 2
보상 필요       Artifact 1
되돌릴 수 없음  외부 장치 효과 0
```

표시 상태는 `취소 요청`, `중지 중`, `취소됨`, `일부 결과 남음`, `취소 불가`로 제한한다. 사용자에게 범용 `롤백` 버튼을 제공하지 않는다. 보상 가능한 효과마다 구체적인 동사형 ActionCard를 제공한다.

## 9. 현재 구현과의 차이

`CTL-004`는 `QUEUED` 현재 Step의 즉시 취소를, `CAN-001`은 `RESERVED` Step의 같은 transaction lease fence·예약 해제를 검증한다. `CAN-002~004`는 실행 중 요청의 immutable 기록, lease fence 뒤 예약 유지, Worker stop·정리 확인 정산과 완료 경합을 검증한다. 다음 계약은 아직 없다.

- ArchivistTurn 취소와 완료 Tool Call 보존
- 불명확한 외부 효과 조정과 Worker 격리
- Command 종류별 보상 계획과 ActionCard

이 제안을 승인하면 위 항목을 원자적인 요구사항과 1:1 테스트로 나눈 뒤 StepState, Command, Event와 화면 계약을 함께 수정한다.

## 10. 승인 뒤 요구사항 분할안

아래 표에서 `CAN-002~004`는 구현된 ID이며 나머지는 검토용 후보다. 후보를 승인하면 각 행을 한 요구사항과 정확히 한 테스트로 연결한다.

| 후보 | 한 가지 관찰 결과 | 테스트 수준 |
|---|---|---|
| `CAN-002` | 실행 중 취소 요청을 immutable 기록·receipt로 남기고 lease를 fence하되 예약을 유지한다 | integration |
| `CAN-003` | Worker stop·정리 확인 뒤 실행 중 Step과 미시작 Job remainder를 정산한다 | integration |
| `CAN-004` | 완료와 취소 경합은 먼저 확정된 revision만 수락하고 늦은 보고를 중복 적용하지 않는다 | integration |
| `CAN-005` | 외부 효과가 남은 취소를 `PARTIAL` 또는 `UNCERTAIN`으로 조정하고 자원을 격리한다 | integration |
| `CAN-006` | ArchivistTurn 취소는 진행 중 생성을 멈추되 완료 Tool Call receipt와 Command를 보존한다 | integration |
| `CMP-001` | 보상 계획은 원래 효과를 보상 가능·승인 필요·불가능으로 분류하고 효과 참조를 반환한다 | unit |
| `CMP-002` | 승인된 보상은 기존 기록을 수정하지 않고 새 Command·Event·Artifact 관계만 추가한다 | integration |
| `CMP-003` | 보상 불가능한 외부 효과는 실행 Command를 만들지 않고 예방 경계와 근거를 반환한다 | unit |

`CAN-001~004` 이후에도 `CAN-005~006`, `CMP-001~003`은 계획 후보이며 구현됐다고 간주하지 않는다.
