<?php

namespace App\Services;

use App\Grpc\Auth\V1\IssueRequest;
use App\Grpc\Auth\V1\VerifyRequest;
use App\Grpc\AuthClient;
use RuntimeException;

/**
 * Issues and verifies access tokens by calling the internal Go auth service over gRPC.
 */
class GrpcTokenService implements TokenService
{
    // gRPC status codes. The Grpc\STATUS_* constants only exist with the grpc extension loaded.
    private const STATUS_OK = 0;

    private const STATUS_UNAUTHENTICATED = 16;

    /**
     * @param  float  $timeoutSeconds  Deadline for every call.
     */
    public function __construct(private AuthClient $client, private float $timeoutSeconds) {}

    public function issue(string $subject): array
    {
        [$response, $status] = $this->client
            ->Issue((new IssueRequest)->setSubject($subject), [], $this->callOptions())
            ->wait();

        $this->assertOk($status, 'Issue');

        return [
            'access_token' => $response->getAccessToken(),
            'expires_in' => $response->getExpiresIn(),
        ];
    }

    public function verify(string $token): ?string
    {
        if ($token === '') {
            return null;
        }

        [$response, $status] = $this->client
            ->Verify((new VerifyRequest)->setAccessToken($token), [], $this->callOptions())
            ->wait();

        if ($status->code === self::STATUS_UNAUTHENTICATED) {
            return null;
        }

        $this->assertOk($status, 'Verify');

        return $response->getSubject();
    }

    /**
     * @return array{timeout: int}
     */
    private function callOptions(): array
    {
        return ['timeout' => (int) ($this->timeoutSeconds * 1_000_000)];
    }

    private function assertOk(object $status, string $method): void
    {
        if ($status->code !== self::STATUS_OK) {
            throw new RuntimeException("Auth service {$method} failed with gRPC status {$status->code}: {$status->details}");
        }
    }
}
