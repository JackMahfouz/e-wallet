<?php

namespace App\Http\Middleware;

use Closure;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Redis;
use Illuminate\Support\Str;
use Symfony\Component\HttpFoundation\Response;

/**
 * Exponential backoff for failed logins, tracked in Redis per email and IP.
 *
 * Each failed login (a 401) is counted. Once the free attempts are used up, further
 * attempts are refused with 429 for a lockout that doubles on every extra failure, up
 * to a cap. A successful login clears the count; otherwise the count decays after a
 * quiet period. The lockout end is stored as a timestamp, so Retry-After is exact.
 */
class LoginBackoff
{
    /**
     * Handle an incoming request.
     *
     * @param  Closure(Request): (Response)  $next
     */
    public function handle(Request $request, Closure $next): Response
    {
        $key = $this->key($request);

        $remainingMs = (int) Redis::hget($key, 'locked_until_ms') - now()->getTimestampMs();

        if ($remainingMs > 0) {
            return response()->json(
                ['message' => 'Too many failed attempts. Try again later.'],
                429,
                ['Retry-After' => (string) (int) ceil($remainingMs / 1000)],
            );
        }

        $response = $next($request);

        if ($response->getStatusCode() === 401) {
            $this->recordFailure($key);
        } elseif ($response->isSuccessful()) {
            Redis::del($key);
        }

        return $response;
    }

    private function key(Request $request): string
    {
        $email = $request->input('email');

        return 'login-backoff:'.sha1(Str::lower(is_string($email) ? $email : '').'|'.$request->ip());
    }

    private function recordFailure(string $key): void
    {
        $backoff = config('auth.login_backoff');
        $excess = Redis::hincrby($key, 'failures', 1) - $backoff['free_attempts'];

        if ($excess > 0) {
            $delaySeconds = min($backoff['base_seconds'] * 2 ** min($excess - 1, 30), $backoff['max_seconds']);

            Redis::hset($key, 'locked_until_ms', now()->getTimestampMs() + $delaySeconds * 1000);
        }

        Redis::expire($key, $backoff['decay_seconds']);
    }
}
