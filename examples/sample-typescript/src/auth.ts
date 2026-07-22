export interface User { username: string }

export class AuthService {
  constructor(private readonly users: Map<string, User>) {}

  login(username: string, password: string): User {
    if (!username || !password) throw new Error("invalid credentials")
    const user = this.users.get(username)
    if (!user) throw new Error("invalid credentials")
    return user
  }
}

export function registerRoutes(app: any, service: AuthService): void {
  app.post("/login", (request: any, response: any) => {
    response.json(service.login(request.body.username, request.body.password))
  })
}
